/*
Copyright (c) YugabyteDB, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	pg_query "github.com/pganalyze/pg_query_go/v5"
	"github.com/samber/lo"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/exp/slices"

	"github.com/yugabyte/yb-voyager/yb-voyager/src/callhome"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/cp"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/metadb"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/srcdb"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/utils"
)

type summaryInfo struct {
	totalCount   int
	invalidCount map[string]bool
	objSet       []string        //TODO: fix it with the set and proper names for INDEX, TRIGGER and POLICY types as "obj_name on table_name"
	details      map[string]bool //any details about the object type
}

type sqlInfo struct {
	objName string
	// SQL statement after removing all new-lines from it. Simplifies analyze-schema regex matching.
	stmt string
	// Formatted SQL statement with new-lines and tabs
	formattedStmt string
	fileName      string
}

var (
	anything   = `.*`
	ws         = `[\s\n\t]+`
	optionalWS = `[\s\n\t]*` //optional white spaces
	//TODO: fix this ident regex for the proper PG identifiers syntax - refer: https://github.com/yugabyte/yb-voyager/pull/1547#discussion_r1629282309
	ident                        = `[a-zA-Z0-9_."-]+`
	ifExists                     = opt("IF", "EXISTS")
	ifNotExists                  = opt("IF", "NOT", "EXISTS")
	optionalCommaSeperatedTokens = `[^,]+(?:,[^,]+){0,}`
	commaSeperatedTokens         = `[^,]+(?:,[^,]+){1,}`
	unqualifiedIdent             = `[a-zA-Z0-9_]+`
	commonClause                 = `[a-zA-Z]+`
	supportedExtensionsOnYB      = []string{
		"adminpack", "amcheck", "autoinc", "bloom", "btree_gin", "btree_gist", "citext", "cube",
		"dblink", "dict_int", "dict_xsyn", "earthdistance", "file_fdw", "fuzzystrmatch", "hll", "hstore",
		"hypopg", "insert_username", "intagg", "intarray", "isn", "lo", "ltree", "moddatetime",
		"orafce", "pageinspect", "pg_buffercache", "pg_cron", "pg_freespacemap", "pg_hint_plan", "pg_prewarm", "pg_stat_monitor",
		"pg_stat_statements", "pg_trgm", "pg_visibility", "pgaudit", "pgcrypto", "pgrowlocks", "pgstattuple", "plpgsql",
		"postgres_fdw", "refint", "seg", "sslinfo", "tablefunc", "tcn", "timetravel", "tsm_system_rows",
		"tsm_system_time", "unaccent", `"uuid-ossp"`, "yb_pg_metrics", "yb_test_extension",
	}
)

func cat(tokens ...string) string {
	str := ""
	for idx, token := range tokens {
		str += token
		nextToken := ""
		if idx < len(tokens)-1 {
			nextToken = tokens[idx+1]
		} else {
			break
		}

		if strings.HasSuffix(token, optionalWS) ||
			strings.HasPrefix(nextToken, optionalWS) || strings.HasSuffix(token, anything) {
			// White space already part of tokens. Nothing to do.
		} else {
			str += ws
		}
	}
	return str
}

func opt(tokens ...string) string {
	return fmt.Sprintf("(%s)?%s", cat(tokens...), optionalWS)
}

func capture(str string) string {
	return "(" + str + ")"
}

func re(tokens ...string) *regexp.Regexp {
	s := cat(tokens...)
	s = "(?i)" + s
	return regexp.MustCompile(s)
}

func parenth(s string) string {
	return `\(` + s + `\)`
}

var (
	analyzeSchemaReportFormat string
	sourceObjList             []string
	schemaAnalysisReport      utils.SchemaReport
	tblPartitions             = make(map[string]bool)
	// key is partitioned table, value is filename where the ADD PRIMARY KEY statement resides
	primaryCons      = make(map[string]string)
	summaryMap       = make(map[string]*summaryInfo)
	multiRegex       = regexp.MustCompile(`([a-zA-Z0-9_\.]+[,|;])`)
	dollarQuoteRegex = regexp.MustCompile(`(\$.*\$)`)
	//TODO: optional but replace every possible space or new line char with [\s\n]+ in all regexs
	gistRegex                 = re("CREATE", "INDEX", ifNotExists, capture(ident), "ON", capture(ident), anything, "USING", "GIST")
	brinRegex                 = re("CREATE", "INDEX", ifNotExists, capture(ident), "ON", capture(ident), anything, "USING", "brin")
	spgistRegex               = re("CREATE", "INDEX", ifNotExists, capture(ident), "ON", capture(ident), anything, "USING", "spgist")
	rtreeRegex                = re("CREATE", "INDEX", ifNotExists, capture(ident), "ON", capture(ident), anything, "USING", "rtree")
	ginRegex                  = re("CREATE", "INDEX", ifNotExists, capture(ident), "ON", capture(ident), anything, "USING", "GIN", capture(optionalCommaSeperatedTokens))
	viewWithCheckRegex        = re("VIEW", capture(ident), anything, "WITH", opt(commonClause), "CHECK", "OPTION")
	rangeRegex                = re("PRECEDING", "and", anything, ":float")
	fetchRegex                = re("FETCH", capture(commonClause), "FROM")
	notSupportedFetchLocation = []string{"FIRST", "LAST", "NEXT", "PRIOR", "RELATIVE", "ABSOLUTE", "NEXT", "FORWARD", "BACKWARD"}
	alterAggRegex             = re("ALTER", "AGGREGATE", capture(ident))
	dropCollRegex             = re("DROP", "COLLATION", ifExists, capture(commaSeperatedTokens))
	dropIdxRegex              = re("DROP", "INDEX", ifExists, capture(commaSeperatedTokens))
	dropViewRegex             = re("DROP", "VIEW", ifExists, capture(commaSeperatedTokens))
	dropSeqRegex              = re("DROP", "SEQUENCE", ifExists, capture(commaSeperatedTokens))
	dropForeignRegex          = re("DROP", "FOREIGN", "TABLE", ifExists, capture(commaSeperatedTokens))
	dropIdxConcurRegex        = re("DROP", "INDEX", "CONCURRENTLY", ifExists, capture(ident))
	trigRefRegex              = re("CREATE", "TRIGGER", capture(ident), anything, "REFERENCING")
	constrTrgRegex            = re("CREATE", "CONSTRAINT", "TRIGGER", capture(ident), "AFTER", capture(commonClause), "ON", capture(ident))
	currentOfRegex            = re("WHERE", "CURRENT", "OF")
	amRegex                   = re("CREATE", "ACCESS", "METHOD", capture(ident))
	idxConcRegex              = re("REINDEX", anything, capture(ident))
	partitionColumnsRegex     = re("CREATE", "TABLE", ifNotExists, capture(ident), parenth(capture(optionalCommaSeperatedTokens)), "PARTITION BY", capture("[A-Za-z]+"), parenth(capture(optionalCommaSeperatedTokens)))
	likeAllRegex              = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "LIKE", anything, "INCLUDING ALL")
	likeRegex                 = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, `\(LIKE`)
	inheritRegex              = re("CREATE", opt(capture(unqualifiedIdent)), "TABLE", ifNotExists, capture(ident), anything, "INHERITS", "[ |(]")
	withOidsRegex             = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "WITH", anything, "OIDS")
	intvlRegex                = re("CREATE", "TABLE", ifNotExists, capture(ident)+`\(`, anything, "interval", "PRIMARY")
	anydataRegex              = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "AnyData", anything)
	anydatasetRegex           = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "AnyDataSet", anything)
	anyTypeRegex              = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "AnyType", anything)
	uriTypeRegex              = re("CREATE", "TABLE", ifNotExists, capture(ident), anything, "URIType", anything)
	//super user role required, language c is errored as unsafe
	cLangRegex = re("CREATE", opt("OR REPLACE"), "FUNCTION", capture(ident), anything, "language c")

	alterOfRegex                    = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "OF", anything)
	alterSchemaRegex                = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "SET SCHEMA")
	createSchemaRegex               = re("CREATE", "SCHEMA", anything, "CREATE", "TABLE")
	alterNotOfRegex                 = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "NOT OF")
	alterColumnStatsRegex           = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "ALTER", "COLUMN", capture(ident), anything, "SET STATISTICS")
	alterColumnStorageRegex         = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "ALTER", "COLUMN", capture(ident), anything, "SET STORAGE")
	alterColumnResetAttributesRegex = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "ALTER", "COLUMN", capture(ident), anything, "RESET", anything)
	alterConstrRegex                = re("ALTER", opt(capture(unqualifiedIdent)), ifExists, "TABLE", capture(ident), anything, "ALTER", "CONSTRAINT")
	setOidsRegex                    = re("ALTER", opt(capture(unqualifiedIdent)), "TABLE", ifExists, capture(ident), anything, "SET WITH OIDS")
	withoutClusterRegex             = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), anything, "SET WITHOUT CLUSTER")
	alterSetRegex                   = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), "SET")
	alterIdxRegex                   = re("ALTER", "INDEX", capture(ident), "SET")
	alterResetRegex                 = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), "RESET")
	alterOptionsRegex               = re("ALTER", opt(capture(unqualifiedIdent)), "TABLE", ifExists, capture(ident), "OPTIONS")
	alterInhRegex                   = re("ALTER", opt(capture(unqualifiedIdent)), "TABLE", ifExists, capture(ident), "INHERIT")
	valConstrRegex                  = re("ALTER", opt(capture(unqualifiedIdent)), "TABLE", ifExists, capture(ident), "VALIDATE CONSTRAINT")
	alterViewRegex                  = re("ALTER", "VIEW", capture(ident))
	dropAttrRegex                   = re("ALTER", "TYPE", capture(ident), "DROP ATTRIBUTE")
	alterTypeRegex                  = re("ALTER", "TYPE", capture(ident))
	alterTblSpcRegex                = re("ALTER", "TABLESPACE", capture(ident), "SET")

	// table partition. partitioned table is the key in tblParts map
	addPrimaryRegex = re("ALTER", "TABLE", opt("ONLY"), ifExists, capture(ident), "ADD CONSTRAINT", capture(ident), "PRIMARY KEY")
	primRegex       = re("CREATE", "FOREIGN", "TABLE", capture(ident)+`\(`, anything, "PRIMARY KEY")
	foreignKeyRegex = re("CREATE", "FOREIGN", "TABLE", capture(ident)+`\(`, anything, "REFERENCES", anything)

	// unsupported SQLs exported by ora2pg
	compoundTrigRegex          = re("CREATE", opt("OR REPLACE"), "TRIGGER", capture(ident), anything, "COMPOUND", anything)
	unsupportedCommentRegex1   = re("--", anything, "(unsupported)")
	packageSupportCommentRegex = re("--", anything, "Oracle package ", "'"+capture(ident)+"'", anything, "please edit to match PostgreSQL syntax")
	unsupportedCommentRegex2   = re("--", anything, "please edit to match PostgreSQL syntax")
	typeUnsupportedRegex       = re("Inherited types are not supported", anything, "replacing with inherited table")
	bulkCollectRegex           = re("BULK COLLECT") // ora2pg unable to convert this oracle feature into a PostgreSQL compatible syntax
	jsonFuncRegex              = re("CREATE", opt("OR REPLACE"), capture(unqualifiedIdent), capture(ident), anything, "JSON_ARRAYAGG")
	alterConvRegex             = re("ALTER", "CONVERSION", capture(ident), anything)
)

const (
	CONVERSION_ISSUE_REASON                     = "CREATE CONVERSION is not supported yet"
	GIN_INDEX_MULTI_COLUMN_ISSUE_REASON         = "Schema contains gin index on multi column which is not supported."
	ADDING_PK_TO_PARTITIONED_TABLE_ISSUE_REASON = "Adding primary key to a partitioned table is not supported yet."
	INHERITANCE_ISSUE_REASON                    = "TABLE INHERITANCE not supported in YugabyteDB"
	CONSTRAINT_TRIGGER_ISSUE_REASON             = "CONSTRAINT TRIGGER not supported yet."
	COMPOUND_TRIGGER_ISSUE_REASON               = "COMPOUND TRIGGER not supported in YugabyteDB."

	STORED_GENERATED_COLUMN_ISSUE_REASON = "Stored generated columns are not supported."
	UNSUPPORTED_EXTENSION_ISSUE          = "This extension is not supported in YugabyteDB by default."
	EXCLUSION_CONSTRAINT_ISSUE           = "Exclusion constraint is not supported yet"
	ALTER_TABLE_DISABLE_RULE_ISSUE       = "ALTER TABLE name DISABLE RULE not supported yet"
	STORAGE_PARAMETERS_DDL_STMT_ISSUE    = "Storage parameters are not supported yet."
	ALTER_TABLE_SET_ATTRUBUTE_ISSUE      = "ALTER TABLE .. ALTER COLUMN .. SET ( attribute = value )	 not supported yet"
	FOREIGN_TABLE_ISSUE_REASON           = "Foreign tables require manual intervention."
	ALTER_TABLE_CLUSTER_ON_ISSUE         = "ALTER TABLE CLUSTER not supported yet."
	DEFERRABLE_CONSTRAINT_ISSUE          = "DEFERRABLE constraints not supported yet"
	POLICY_ROLE_ISSUE                    = "Policy require roles to be created."
	VIEW_CHECK_OPTION_ISSUE              = "Schema containing VIEW WITH CHECK OPTION is not supported yet."
	UNSUPPORTED_DATATYPE                 = "Unsupported datatype"
	UNSUPPORTED_PG_SYNTAX                = "Unsupported PG syntax"

	GIST_INDEX_ISSUE_REASON                        = "Schema contains GIST index which is not supported."
	GIN_INDEX_DETAILS                              = "There are some GIN indexes present in the schema, but GIN indexes are partially supported in YugabyteDB as mentioned in (https://github.com/yugabyte/yugabyte-db/issues/7850) so take a look and modify them if not supported."
	UNSUPPORTED_DATATYPES_FOR_LIVE_MIGRATION_ISSUE = "There are some data types in the schema that are not supported by live migration of data. These columns will be excluded when exporting and importing data in live migration workflows."
)

// Reports one case in JSON
func reportCase(filePath string, reason string, ghIssue string, suggestion string, objType string, objName string, sqlStmt string, issueType string, docsLink string) {
	var issue utils.Issue
	issue.FilePath = filePath
	issue.Reason = reason
	issue.GH = ghIssue
	issue.Suggestion = suggestion
	issue.ObjectType = objType
	issue.ObjectName = objName
	issue.SqlStatement = sqlStmt
	issue.IssueType = issueType
	if sourceDBType == POSTGRESQL {
		issue.DocsLink = docsLink
	}

	schemaAnalysisReport.Issues = append(schemaAnalysisReport.Issues, issue)
}

func reportAddingPrimaryKey(fpath string, objType string, tbl string, line string) {
	reportCase(fpath, ADDING_PK_TO_PARTITIONED_TABLE_ISSUE_REASON,
		"https://github.com/yugabyte/yugabyte-db/issues/10074", "", objType, tbl, line, MIGRATION_CAVEATS, ADDING_PK_TO_PARTITIONED_TABLE_DOC_LINK)
}

func reportBasedOnComment(comment int, fpath string, issue string, suggestion string, objName string, objType string, line string) {
	if comment == 1 {
		reportCase(fpath, "Unsupported, please edit to match PostgreSQL syntax", "https://github.com/yugabyte/yb-voyager/issues/1625", suggestion, objType, objName, line, UNSUPPORTED_FEATURES, "")
		summaryMap[objType].invalidCount[objName] = true
	} else if comment == 2 {
		// reportCase(fpath, "PACKAGE in oracle are exported as Schema, please review and edit to match PostgreSQL syntax if required, Package is "+objName, issue, suggestion, objType)
		summaryMap["PACKAGE"].objSet = append(summaryMap["PACKAGE"].objSet, objName)
	} else if comment == 3 {
		reportCase(fpath, "SQLs in file might be unsupported please review and edit to match PostgreSQL syntax if required. ", "https://github.com/yugabyte/yb-voyager/issues/1625", suggestion, objType, objName, line, UNSUPPORTED_FEATURES, "")
	} else if comment == 4 {
		summaryMap[objType].details["Inherited Types are present which are not supported in PostgreSQL syntax, so exported as Inherited Tables"] = true
	}

}

// return summary about the schema objects like tables, indexes, functions, sequences
func reportSchemaSummary(sourceDBConf *srcdb.Source) utils.SchemaSummary {
	var schemaSummary utils.SchemaSummary

	if !tconf.ImportMode && sourceDBConf != nil { // this info is available only if we are exporting from source
		schemaSummary.DBName = sourceDBConf.DBName
		schemaSummary.SchemaNames = strings.Split(sourceDBConf.Schema, "|")
		schemaSummary.DBVersion = sourceDBConf.DBVersion
	}

	addSummaryDetailsForIndexes()
	for _, objType := range sourceObjList {
		if summaryMap[objType].totalCount == 0 {
			continue
		}

		var dbObject utils.DBObject
		dbObject.ObjectType = objType
		dbObject.TotalCount = summaryMap[objType].totalCount
		dbObject.InvalidCount = len(lo.Keys(summaryMap[objType].invalidCount))
		dbObject.ObjectNames = strings.Join(summaryMap[objType].objSet, ", ")
		dbObject.Details = strings.Join(lo.Keys(summaryMap[objType].details), "\n")
		schemaSummary.DBObjects = append(schemaSummary.DBObjects, dbObject)
	}
	filePath := filepath.Join(exportDir, "schema", "uncategorized.sql")
	if utils.FileOrFolderExists(filePath) {
		note := fmt.Sprintf("Review and manually import the DDL statements from the file %s", filePath)
		schemaSummary.Notes = append(schemaAnalysisReport.SchemaSummary.Notes, note)
	}
	schemaSummary.MigrationComplexity = getMigrationComplexity(sourceDBConf.DBType, schemaDir, schemaAnalysisReport)
	return schemaSummary
}

func addSummaryDetailsForIndexes() {
	var indexesInfo []utils.IndexInfo
	found, err := metaDB.GetJsonObject(nil, metadb.SOURCE_INDEXES_INFO_KEY, &indexesInfo)
	if err != nil {
		utils.ErrExit("analyze schema report summary: load indexes info: %s", err)
	}
	if !found {
		return
	}
	exportedIndexes := summaryMap["INDEX"].objSet
	unexportedIdxsMsg := "Indexes which are neither exported by yb-voyager as they are unsupported in YB and needs to be handled manually:\n"
	unexportedIdxsPresent := false
	for _, indexInfo := range indexesInfo {
		sourceIdxName := indexInfo.TableName + "_" + strings.Join(indexInfo.Columns, "_")
		if !slices.Contains(exportedIndexes, strings.ToLower(sourceIdxName)) {
			unexportedIdxsPresent = true
			unexportedIdxsMsg += fmt.Sprintf("\t\tIndex Name=%s, Index Type=%s\n", indexInfo.IndexName, indexInfo.IndexType)
		}
	}
	if unexportedIdxsPresent {
		summaryMap["INDEX"].details[unexportedIdxsMsg] = true
	}
}

// Checks Whether there is a GIN index
/*
Following type of SQL queries are being taken care of by this function -
	1. CREATE INDEX index_name ON table_name USING gin(column1, column2 ...)
	2. CREATE INDEX index_name ON table_name USING gin(column1 [ASC/DESC/HASH])
	3. CREATE EXTENSION btree_gin;
*/
func checkGin(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		matchGin := ginRegex.FindStringSubmatch(sqlInfo.stmt)
		if matchGin != nil {
			columnsFromGin := strings.Trim(matchGin[4], `()`)
			columnList := strings.Split(columnsFromGin, ",")
			if len(columnList) > 1 {
				summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", matchGin[2], matchGin[3])] = true
				reportCase(fpath, "Schema contains gin index on multi column which is not supported.",
					"https://github.com/yugabyte/yugabyte-db/issues/7850", "", "INDEX", fmt.Sprintf("%s ON %s", matchGin[2], matchGin[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, GIN_INDEX_MULTI_COLUMN_DOC_LINK)
			} else {
				if strings.Contains(strings.ToUpper(columnList[0]), "ASC") || strings.Contains(strings.ToUpper(columnList[0]), "DESC") || strings.Contains(strings.ToUpper(columnList[0]), "HASH") {
					summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", matchGin[2], matchGin[3])] = true
					reportCase(fpath, "Schema contains gin index on column with ASC/DESC/HASH Clause which is not supported.",
						"https://github.com/yugabyte/yugabyte-db/issues/7850", "", "INDEX", fmt.Sprintf("%s ON %s", matchGin[2], matchGin[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, GIN_INDEX_DIFFERENT_ISSUE_DOC_LINK)
				}
			}
		}
		if strings.Contains(strings.ToLower(sqlInfo.stmt), "using gin") {
			summaryMap["INDEX"].details[GIN_INDEX_DETAILS] = true
		}
	}
}

// Checks whether there is gist index
func checkGist(sqlInfoArr []sqlInfo, fpath string) {
	//TODO: add other index types in assessment
	for _, sqlInfo := range sqlInfoArr {
		if idx := gistRegex.FindStringSubmatch(sqlInfo.stmt); idx != nil {
			summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", idx[2], idx[3])] = true
			reportCase(fpath, GIST_INDEX_ISSUE_REASON,
				"https://github.com/YugaByte/yugabyte-db/issues/1337", "", "INDEX", fmt.Sprintf("%s ON %s", idx[2], idx[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, GIST_INDEX_DOC_LINK)
		} else if idx := brinRegex.FindStringSubmatch(sqlInfo.stmt); idx != nil {
			summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", idx[2], idx[3])] = true
			reportCase(fpath, "index method 'brin' not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1337", "", "INDEX", fmt.Sprintf("%s ON %s", idx[2], idx[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if idx := spgistRegex.FindStringSubmatch(sqlInfo.stmt); idx != nil {
			summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", idx[2], idx[3])] = true
			reportCase(fpath, "index method 'spgist' not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1337", "", "INDEX", fmt.Sprintf("%s ON %s", idx[2], idx[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if idx := rtreeRegex.FindStringSubmatch(sqlInfo.stmt); idx != nil {
			summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", idx[2], idx[3])] = true
			reportCase(fpath, "index method 'rtree' is superceded by 'gist' which is not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1337", "", "INDEX", fmt.Sprintf("%s ON %s", idx[2], idx[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		}
	}
}

func checkForeignTable(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlStmtInfo := range sqlInfoArr {
		parseTree, err := pg_query.Parse(sqlStmtInfo.stmt)
		if err != nil {
			utils.ErrExit("failed to parse the stmt %v: %v", sqlStmtInfo.stmt, err)
		}
		createForeignTableNode, isForeignTable := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_CreateForeignTableStmt)
		if isForeignTable {
			schemaName := createForeignTableNode.CreateForeignTableStmt.BaseStmt.Relation.Schemaname
			tableName := createForeignTableNode.CreateForeignTableStmt.BaseStmt.Relation.Relname
			serverName := createForeignTableNode.CreateForeignTableStmt.Servername
			summaryMap["FOREIGN TABLE"].invalidCount[sqlStmtInfo.objName] = true
			objName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
			reportCase(fpath, FOREIGN_TABLE_ISSUE_REASON, "https://github.com/yugabyte/yb-voyager/issues/1627",
				fmt.Sprintf("SERVER '%s', and USER MAPPING should be created manually on the target to create and use the foreign table", serverName), "FOREIGN TABLE", objName, sqlStmtInfo.stmt, MIGRATION_CAVEATS, FOREIGN_TABLE_DOC_LINK)
		}
	}
}

func checkStmtsUsingParser(sqlInfoArr []sqlInfo, fpath string, objType string) {
	for _, sqlStmtInfo := range sqlInfoArr {
		parseTree, err := pg_query.Parse(sqlStmtInfo.stmt)
		if err != nil { //if the Stmt is not already report by any of the regexes
			if !summaryMap[objType].invalidCount[sqlStmtInfo.objName] {
				reason := fmt.Sprintf("%s - '%s'", UNSUPPORTED_PG_SYNTAX, err.Error())
				reportCase(fpath, reason, "https://github.com/yugabyte/yb-voyager/issues/1625",
					"Fix the schema as per PG syntax", objType, sqlStmtInfo.objName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
			}
			continue
		}
		createTableNode, isCreateTable := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_CreateStmt)
		alterTableNode, isAlterTable := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_AlterTableStmt)
		createIndexNode, isCreateIndex := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_IndexStmt)
		createPolicyNode, isCreatePolicy := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_CreatePolicyStmt)

		if objType == TABLE && isCreateTable {
			reportGeneratedStoredColumnTables(createTableNode, sqlStmtInfo, fpath)
			reportExclusionConstraintCreateTable(createTableNode, sqlStmtInfo, fpath)
			reportDeferrableConstraintCreateTable(createTableNode, sqlStmtInfo, fpath)
			reportXMLAndXIDDatatype(createTableNode, sqlStmtInfo, fpath)
		}
		if isAlterTable {
			reportAlterTableVariants(alterTableNode, sqlStmtInfo, fpath, objType)
			reportExclusionConstraintAlterTable(alterTableNode, sqlStmtInfo, fpath)
			reportDeferrableConstraintAlterTable(alterTableNode, sqlStmtInfo, fpath)
		}
		if isCreateIndex {
			reportCreateIndexStorageParameter(createIndexNode, sqlStmtInfo, fpath)
		}

		if isCreatePolicy {
			reportPolicyRequireRolesOrGrants(createPolicyNode, sqlStmtInfo, fpath)
		}
	}
}

func reportPolicyRequireRolesOrGrants(createPolicyNode *pg_query.Node_CreatePolicyStmt, sqlStmtInfo sqlInfo, fpath string) {
	policyName := createPolicyNode.CreatePolicyStmt.GetPolicyName()
	roles := createPolicyNode.CreatePolicyStmt.GetRoles()
	relname := createPolicyNode.CreatePolicyStmt.GetTable()
	schemaName := relname.Schemaname
	tableName := relname.Relname
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	roleNames := make([]string, 0)
	/*
		e.g. CREATE POLICY P ON tbl1 TO regress_rls_eve, regress_rls_frank USING (true);
		stmt:{create_policy_stmt:{policy_name:"p" table:{relname:"tbl1" inh:true relpersistence:"p" location:20} cmd_name:"all"
		permissive:true roles:{role_spec:{roletype:ROLESPEC_CSTRING rolename:"regress_rls_eve" location:28}} roles:{role_spec:
		{roletype:ROLESPEC_CSTRING rolename:"regress_rls_frank" location:45}} qual:{a_const:{boolval:{boolval:true} location:70}}}}
		stmt_len:75

		here role_spec of each roles is managing the roles related information in a POLICY DDL if any, so we can just check if there is
		a role name available in it which means there is a role associated with this DDL. Hence report it.

	*/
	for _, role := range roles {
		roleName := role.GetRoleSpec().GetRolename() // only in case there is role associated with a policy it will error out in schema migration
		if roleName != "" {
			//this means there is some role or grants used in this Policy, so detecting it
			roleNames = append(roleNames, roleName)
		}
	}
	if len(roleNames) > 0 {
		policyNameWithTable := fmt.Sprintf("%s ON %s", policyName, fullyQualifiedName)
		summaryMap["POLICY"].invalidCount[policyNameWithTable] = true
		reportCase(fpath, fmt.Sprintf("%s Users - (%s)", POLICY_ROLE_ISSUE, strings.Join(roleNames, ",")), "https://github.com/yugabyte/yb-voyager/issues/1655",
			"Users/Grants are not migrated during the schema migration. Create the Users manually to make the policies work",
			"POLICY", policyNameWithTable, sqlStmtInfo.formattedStmt, MIGRATION_CAVEATS, POLICY_DOC_LINK)
	}
}

func reportXMLAndXIDDatatype(createTableNode *pg_query.Node_CreateStmt, sqlStmtInfo sqlInfo, fpath string) {
	schemaName := createTableNode.CreateStmt.Relation.Schemaname
	tableName := createTableNode.CreateStmt.Relation.Relname
	columns := createTableNode.CreateStmt.TableElts
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	for _, column := range columns {
		/*
			e.g. CREATE TABLE test_xml_type(id int, data xml);
			relation:{relname:"test_xml_type" inh:true relpersistence:"p" location:15} table_elts:{column_def:{colname:"id"
			type_name:{names:{string:{sval:"pg_catalog"}} names:{string:{sval:"int4"}} typemod:-1 location:32}
			is_local:true location:29}} table_elts:{column_def:{colname:"data" type_name:{names:{string:{sval:"xml"}}
			typemod:-1 location:42} is_local:true location:37}} oncommit:ONCOMMIT_NOOP}}

			here checking the type of each column as type definition can be a list names for types which are native e.g. int
			it has type names - [pg_catalog, int4] both to determine but for complex types like text,json or xml etc. if doesn't have
			info about pg_catalog. so checking the 0th only in case XML/XID to determine the type and report
		*/
		if column.GetColumnDef() != nil {
			typeName := ""
			if len(column.GetColumnDef().GetTypeName().GetNames()) > 0 {
				typeName = column.GetColumnDef().GetTypeName().GetNames()[0].GetString_().Sval // 0th index as for these non-native types pg_catalog won't be present on first location
			}
			colName := column.GetColumnDef().GetColname()
			reason := fmt.Sprintf("%s - %s on column - %s", UNSUPPORTED_DATATYPE, typeName, colName)
			if typeName == "xml" {
				summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
				reportCase(fpath, reason, "https://github.com/yugabyte/yugabyte-db/issues/1043",
					"Data ingestion is not supported for this type in YugabyteDB so handle this type in different way. Refer link for more details - <LINK DOC>",
					"TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_DATATYPES, XML_DATATYPE_DOC_LINK)
			}
			if typeName == "xid" {
				summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
				reportCase(fpath, reason, "https://github.com/yugabyte/yugabyte-db/issues/15638",
					"Functions for this type e.g. txid_current are not supported in YugabyteDB yet",
					"TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_DATATYPES, "")
			}
		}
	}
}

var deferrableConstraintsList = []pg_query.ConstrType{
	pg_query.ConstrType_CONSTR_ATTR_DEFERRABLE,
	pg_query.ConstrType_CONSTR_ATTR_DEFERRED,
	pg_query.ConstrType_CONSTR_ATTR_IMMEDIATE,
}

func reportDeferrableConstraintCreateTable(createTableNode *pg_query.Node_CreateStmt, sqlStmtInfo sqlInfo, fpath string) {
	schemaName := createTableNode.CreateStmt.Relation.Schemaname
	tableName := createTableNode.CreateStmt.Relation.Relname
	columns := createTableNode.CreateStmt.TableElts
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	for _, column := range columns {
		/*
			e.g. create table unique_def_test(id int UNIQUE DEFERRABLE, c1 int);
			create_stmt:{relation:{relname:"unique_def_test"  inh:true  relpersistence:"p"  location:15}
			table_elts:{column_def:{colname:"id"  type_name:{names:{string:{sval:"pg_catalog"}}  names:{string:{sval:"int4"}}
			typemod:-1  location:34}  is_local:true  constraints:{constraint:{contype:CONSTR_UNIQUE  location:38}}
			constraints:{constraint:{contype:CONSTR_ATTR_DEFERRABLE  location:45}}  location:31}}  ....

			here checking the case where this clause is in column definition so iterating over each column_def and in that
			constraint type has deferrable or not and also it should not be a foreign constraint as Deferrable on FKs are
			supported.
		*/
		if column.GetColumnDef() != nil {
			constraints := column.GetColumnDef().GetConstraints()
			if constraints != nil {
				isForeignConstraint := false
				isDeferrable := false
				for _, constraint := range constraints {
					if constraint.GetConstraint().Contype == pg_query.ConstrType_CONSTR_FOREIGN {
						isForeignConstraint = true
					} else if slices.Contains(deferrableConstraintsList, constraint.GetConstraint().Contype) {
						isDeferrable = true
					}
				}
				if !isForeignConstraint && isDeferrable {
					summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
					reportCase(fpath, DEFERRABLE_CONSTRAINT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/1709",
						"Remove these constraints from the exported schema and make the necessary changes to the application before pointing it to target",
						"TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, DEFERRABLE_CONSTRAINT_DOC_LINK)
				}
			}
		} else if column.GetConstraint() != nil {
			/*
				e.g. create table uniquen_def_test1(id int, c1 int, UNIQUE(id) DEFERRABLE INITIALLY DEFERRED);
				{create_stmt:{relation:{relname:"unique_def_test1"  inh:true  relpersistence:"p"  location:80}  table_elts:{column_def:{colname:"id"
				type_name:{....  names:{string:{sval:"int4"}}  typemod:-1  location:108}  is_local:true  location:105}}
				table_elts:{constraint:{contype:CONSTR_UNIQUE  deferrable:true  initdeferred:true location:113  keys:{string:{sval:"id"}}}} ..

				here checking the case where this constraint is at the at the end as a constraint only, so checking deferrable field in constraint
				in case of its not a FK.
			*/
			if column.GetConstraint().Deferrable && column.GetConstraint().Contype != pg_query.ConstrType_CONSTR_FOREIGN {
				summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
				reportCase(fpath, DEFERRABLE_CONSTRAINT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/1709",
					"Remove these constraints from the exported schema and make the neccessary changes to the application to work on target seamlessly",
					"TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, DEFERRABLE_CONSTRAINT_DOC_LINK)
			}
		}
	}
}

func reportDeferrableConstraintAlterTable(alterTableNode *pg_query.Node_AlterTableStmt, sqlStmtInfo sqlInfo, fpath string) {
	schemaName := alterTableNode.AlterTableStmt.Relation.Schemaname
	tableName := alterTableNode.AlterTableStmt.Relation.Relname
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)

	alterCmd := alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd()
	/*
		e.g. ALTER TABLE ONLY public.users ADD CONSTRAINT users_email_key UNIQUE (email) DEFERRABLE;
		alter_table_cmd:{subtype:AT_AddConstraint  def:{constraint:{contype:CONSTR_UNIQUE  conname:"users_email_key"
		deferrable:true  location:196  keys:{string:{sval:"email"}}}}  behavior:DROP_RESTRICT}}  objtype:OBJECT_TABLE}}

		similar to CREATE table 2nd case where constraint is at the end of column definitions mentioning the constraint only
		so here as well while adding constraint checking the type of constraint and the deferrable field of it.
	*/
	constraint := alterCmd.GetDef().GetConstraint()
	if constraint != nil && constraint.Deferrable && constraint.Contype != pg_query.ConstrType_CONSTR_FOREIGN {
		summaryMap["TABLE"].invalidCount[fullyQualifiedName] = true
		reportCase(fpath, DEFERRABLE_CONSTRAINT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/1709",
			"Remove these constraints from the exported schema and make the neccessary changes to the application to work on target seamlessly",
			"TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, DEFERRABLE_CONSTRAINT_DOC_LINK)
	}
}

func reportExclusionConstraintCreateTable(createTableNode *pg_query.Node_CreateStmt, sqlStmtInfo sqlInfo, fpath string) {

	schemaName := createTableNode.CreateStmt.Relation.Schemaname
	tableName := createTableNode.CreateStmt.Relation.Relname
	columns := createTableNode.CreateStmt.TableElts
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	/*
		e.g. CREATE TABLE "Test"(
				id int,
				room_id int,
				time_range trange,
				EXCLUDE USING gist (room_id WITH =, time_range WITH &&)
			);
		create_stmt:{relation:{relname:"Test" inh:true relpersistence:"p" location:14} table_elts:{column_def:
		... table_elts:{column_def:{colname:"time_range" type_name:{names:{string:{sval:"trange"}}
		typemod:-1 location:59} is_local:true location:48}} table_elts:{constraint:{contype:CONSTR_EXCLUSION
		location:69 exclusions:{list:{items:{index_elem:{name:"room_id" ordering:SORTBY_DEFAULT ...

		here we are iterating over all the table_elts - table elements and which are comma separated column info in
		the DDL so each column has column_def(column definition) in the parse tree but in case it is a constraint, the column_def
		is nil.

	*/
	for _, column := range columns {
		//In case CREATE DDL has EXCLUDE USING gist(room_id '=', time_range WITH &&) - it will be included in columns but won't have columnDef as its a constraint
		if column.GetColumnDef() == nil && column.GetConstraint() != nil {
			if column.GetConstraint().Contype == pg_query.ConstrType_CONSTR_EXCLUSION {
				summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
				reportCase(fpath, EXCLUSION_CONSTRAINT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/3944",
					"Refer docs link for details on possible workaround", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, EXCLUSION_CONSTRAINT_DOC_LINK)
			}
		}
	}
}

func reportCreateIndexStorageParameter(createIndexNode *pg_query.Node_IndexStmt, sqlStmtInfo sqlInfo, fpath string) {
	indexName := createIndexNode.IndexStmt.GetIdxname()
	relName := createIndexNode.IndexStmt.GetRelation()
	schemaName := relName.GetSchemaname()
	tableName := relName.GetRelname()
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	/*
		e.g. CREATE INDEX idx on table_name(id) with (fillfactor='70');
		index_stmt:{idxname:"idx" relation:{relname:"table_name" inh:true relpersistence:"p" location:21} access_method:"btree"
		index_params:{index_elem:{name:"id" ordering:SORTBY_DEFAULT nulls_ordering:SORTBY_NULLS_DEFAULT}}
		options:{def_elem:{defname:"fillfactor" arg:{string:{sval:"70"}} ...
		here again similar to ALTER table Storage parameters options is the high level field in for WITH options.
	*/
	if len(createIndexNode.IndexStmt.GetOptions()) > 0 {
		//YB doesn't support any storage parameters from PG yet refer -
		//https://docs.yugabyte.com/preview/api/ysql/the-sql-language/statements/ddl_create_table/#storage-parameters-1
		summaryMap["INDEX"].invalidCount[fmt.Sprintf("%s ON %s", indexName, fullyQualifiedName)] = true
		reportCase(fpath, STORAGE_PARAMETERS_DDL_STMT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/23467",
			"Remove the storage parameters from the DDL", "INDEX", indexName, sqlStmtInfo.stmt, UNSUPPORTED_FEATURES, STORAGE_PARAMETERS_DDL_STMT_DOC_LINK)
	}
}

func reportAlterTableVariants(alterTableNode *pg_query.Node_AlterTableStmt, sqlStmtInfo sqlInfo, fpath string, objType string) {
	schemaName := alterTableNode.AlterTableStmt.Relation.Schemaname
	tableName := alterTableNode.AlterTableStmt.Relation.Relname
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	// this will the list of items in the SET (attribute=value, ..)
	/*
		e.g. alter table test_1 alter column col1 set (attribute_option=value);
		cmds:{alter_table_cmd:{subtype:AT_SetOptions name:"col1" def:{list:{items:{def_elem:{defname:"attribute_option"
		arg:{type_name:{names:{string:{sval:"value"}} typemod:-1 location:263}} defaction:DEFELEM_UNSPEC location:246}}}}...
		for set attribute issue we will the type of alter setting the options and in the 'def' definition field which has the
		information of the type, we will check if there is any list which will only present in case there is syntax like <SubTYPE> (...)
	*/
	if alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetSubtype() == pg_query.AlterTableType_AT_SetOptions &&
		len(alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetDef().GetList().GetItems()) > 0 {
		reportCase(fpath, ALTER_TABLE_SET_ATTRUBUTE_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/1124",
			"Remove it from the exported schema", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, UNSUPPORTED_ALTER_VARIANTS_DOC_LINK)
	}

	/*
		e.g. alter table test add constraint uk unique(id) with (fillfactor='70');
		alter_table_cmd:{subtype:AT_AddConstraint def:{constraint:{contype:CONSTR_UNIQUE conname:"asd" location:292
		keys:{string:{sval:"id"}} options:{def_elem:{defname:"fillfactor" arg:{string:{sval:"70"}}...
		Similarly here we are trying to get the constraint if any and then get the options field which is WITH options
		in this case only so checking that for this case.
	*/

	if alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetSubtype() == pg_query.AlterTableType_AT_AddConstraint &&
		len(alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetDef().GetConstraint().GetOptions()) > 0 {
		reportCase(fpath, STORAGE_PARAMETERS_DDL_STMT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/23467",
			"Remove the storage parameters from the DDL", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, STORAGE_PARAMETERS_DDL_STMT_DOC_LINK)
	}

	/*
		e.g. ALTER TABLE example DISABLE example_rule;
		cmds:{alter_table_cmd:{subtype:AT_DisableRule name:"example_rule" behavior:DROP_RESTRICT}} objtype:OBJECT_TABLE}}
		checking the subType is sufficient in this case
	*/
	if alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetSubtype() == pg_query.AlterTableType_AT_DisableRule {
		ruleName := alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetName()
		reportCase(fpath, ALTER_TABLE_DISABLE_RULE_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/1124",
			fmt.Sprintf("Remove this and the rule '%s' from the exported schema to be not enabled on the table.", ruleName), "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, UNSUPPORTED_ALTER_VARIANTS_DOC_LINK)
	}
	/*
		e.g. ALTER TABLE example CLUSTER ON idx;
		stmt:{alter_table_stmt:{relation:{relname:"example" inh:true relpersistence:"p" location:13} 
		cmds:{alter_table_cmd:{subtype:AT_ClusterOn name:"idx" behavior:DROP_RESTRICT}} objtype:OBJECT_TABLE}} stmt_len:32
	
	*/
	if alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd().GetSubtype() == pg_query.AlterTableType_AT_ClusterOn {
		reportCase(fpath, ALTER_TABLE_CLUSTER_ON_ISSUE,
			"https://github.com/YugaByte/yugabyte-db/issues/1124", "Remove it from the exported schema.", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, UNSUPPORTED_ALTER_VARIANTS_DOC_LINK)
	}

}

func reportExclusionConstraintAlterTable(alterTableNode *pg_query.Node_AlterTableStmt, sqlStmtInfo sqlInfo, fpath string) {

	schemaName := alterTableNode.AlterTableStmt.Relation.Schemaname
	tableName := alterTableNode.AlterTableStmt.Relation.Relname
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	alterCmd := alterTableNode.AlterTableStmt.Cmds[0].GetAlterTableCmd()
	/*
		e.g. ALTER TABLE ONLY public.meeting ADD CONSTRAINT no_time_overlap EXCLUDE USING gist (room_id WITH =, time_range WITH &&);
		cmds:{alter_table_cmd:{subtype:AT_AddConstraint def:{constraint:{contype:CONSTR_EXCLUSION conname:"no_time_overlap" location:41
		here again same checking the definition of the alter stmt if it has constraint and checking its type
	*/
	constraint := alterCmd.GetDef().GetConstraint()
	if alterCmd.Subtype == pg_query.AlterTableType_AT_AddConstraint && constraint.Contype == pg_query.ConstrType_CONSTR_EXCLUSION {
		summaryMap["TABLE"].invalidCount[fullyQualifiedName] = true
		reportCase(fpath, EXCLUSION_CONSTRAINT_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/3944",
			"Refer docs link for details on possible workaround", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, EXCLUSION_CONSTRAINT_DOC_LINK)
	}
}

func reportGeneratedStoredColumnTables(createTableNode *pg_query.Node_CreateStmt, sqlStmtInfo sqlInfo, fpath string) {
	schemaName := createTableNode.CreateStmt.Relation.Schemaname
	tableName := createTableNode.CreateStmt.Relation.Relname
	columns := createTableNode.CreateStmt.TableElts
	var generatedColumns []string
	for _, column := range columns {
		//In case CREATE DDL has PRIMARY KEY(column_name) - it will be included in columns but won't have columnDef as its a constraint
		if column.GetColumnDef() != nil {
			constraints := column.GetColumnDef().Constraints
			for _, constraint := range constraints {
				if constraint.GetConstraint().Contype == pg_query.ConstrType_CONSTR_GENERATED {
					generatedColumns = append(generatedColumns, column.GetColumnDef().Colname)
				}
			}
		}
	}
	fullyQualifiedName := lo.Ternary(schemaName != "", schemaName+"."+tableName, tableName)
	if len(generatedColumns) > 0 {
		summaryMap["TABLE"].invalidCount[sqlStmtInfo.objName] = true
		reportCase(fpath, STORED_GENERATED_COLUMN_ISSUE_REASON+fmt.Sprintf(" Generated Columns: (%s)", strings.Join(generatedColumns, ",")),
			"https://github.com/yugabyte/yugabyte-db/issues/10695", "Using Triggers to update the generated columns is one way to work around this issue, refer docs link for more details.", "TABLE", fullyQualifiedName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, GENERATED_STORED_COLUMN_DOC_LINK)
	}
}

// Checks compatibility of views
func checkViews(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		/*if dropMatViewRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP MATERIALIZED VIEW not supported yet.",a
				"https://github.com/YugaByte/yugabyte-db/issues/10102", "")
		} else if view := matViewRegex.FindStringSubmatch(sqlInfo.stmt); view != nil {
			reportCase(fpath, "Schema contains materialized view which is not supported. The view is: "+view[1],
				"https://github.com/yugabyte/yugabyte-db/issues/10102", "")
		} else */
		if view := viewWithCheckRegex.FindStringSubmatch(sqlInfo.stmt); view != nil {
			summaryMap["VIEW"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, VIEW_CHECK_OPTION_ISSUE, "https://github.com/yugabyte/yugabyte-db/issues/22716",
				"Use Trigger with INSTEAD OF clause on INSERT/UPDATE on view to get this functionality", "VIEW", view[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, VIEW_CHECK_OPTION_DOC_LINK)
		}
	}
}

// Separates the input line into multiple statements which are accepted by YB.
func separateMultiObj(objType string, line string) string {
	indexes := multiRegex.FindAllStringSubmatchIndex(line, -1)
	suggestion := ""
	for _, match := range indexes {
		start := match[2]
		end := match[3]
		obj := strings.Replace(line[start:end], ",", ";", -1)
		suggestion += objType + " " + obj
	}
	return suggestion
}

// Checks compatibility of SQL statements
func checkSql(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		if rangeRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath,
				"RANGE with offset PRECEDING/FOLLOWING is not supported for column type numeric and offset type double precision",
				"https://github.com/yugabyte/yugabyte-db/issues/10692", "", "TABLE", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if stmt := fetchRegex.FindStringSubmatch(sqlInfo.stmt); stmt != nil {
			location := strings.ToUpper(stmt[1])
			if slices.Contains(notSupportedFetchLocation, location) {
				summaryMap["PROCEDURE"].invalidCount[sqlInfo.objName] = true
				reportCase(fpath, "This FETCH clause might not be supported yet", "https://github.com/YugaByte/yugabyte-db/issues/6514",
					"Please verify the DDL on your YugabyteDB version before proceeding", "CURSOR", sqlInfo.objName, sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
			}
		} else if stmt := alterAggRegex.FindStringSubmatch(sqlInfo.stmt); stmt != nil {
			reportCase(fpath, "ALTER AGGREGATE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/2717", "", "AGGREGATE", stmt[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if dropCollRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP multiple objects not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/880", separateMultiObj("DROP COLLATION", sqlInfo.formattedStmt), "COLLATION", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if dropIdxRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP multiple objects not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/880", separateMultiObj("DROP INDEX", sqlInfo.formattedStmt), "INDEX", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if dropViewRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP multiple objects not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/880", separateMultiObj("DROP VIEW", sqlInfo.formattedStmt), "VIEW", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if dropSeqRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP multiple objects not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/880", separateMultiObj("DROP SEQUENCE", sqlInfo.formattedStmt), "SEQUENCE", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if dropForeignRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "DROP multiple objects not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/880", separateMultiObj("DROP FOREIGN TABLE", sqlInfo.formattedStmt), "FOREIGN TABLE", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if idx := dropIdxConcurRegex.FindStringSubmatch(sqlInfo.stmt); idx != nil {
			reportCase(fpath, "DROP INDEX CONCURRENTLY not supported yet",
				"https://github.com/yugabyte/yugabyte-db/issues/22717", "", "INDEX", idx[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if trig := trigRefRegex.FindStringSubmatch(sqlInfo.stmt); trig != nil {
			summaryMap["TRIGGER"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "REFERENCING clause (transition tables) not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1668", "", "TRIGGER", trig[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if trig := constrTrgRegex.FindStringSubmatch(sqlInfo.stmt); trig != nil {
			reportCase(fpath, CONSTRAINT_TRIGGER_ISSUE_REASON,
				"https://github.com/YugaByte/yugabyte-db/issues/1709", "", "TRIGGER", fmt.Sprintf("%s ON %s", trig[1], trig[3]), sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, CONSTRAINT_TRIGGER_DOC_LINK)
		} else if currentOfRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "WHERE CURRENT OF not supported yet", "https://github.com/YugaByte/yugabyte-db/issues/737", "", "CURSOR", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if bulkCollectRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "BULK COLLECT keyword of oracle is not converted into PostgreSQL compatible syntax", "https://github.com/yugabyte/yb-voyager/issues/1539", "", "", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		}
	}
}

// Checks unsupported DDL statements
func checkDDL(sqlInfoArr []sqlInfo, fpath string, objType string) {

	for _, sqlInfo := range sqlInfoArr {
		if am := amRegex.FindStringSubmatch(sqlInfo.stmt); am != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "CREATE ACCESS METHOD is not supported.",
				"https://github.com/yugabyte/yugabyte-db/issues/10693", "", "ACCESS METHOD", am[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := idxConcRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "REINDEX is not supported.",
				"https://github.com/yugabyte/yugabyte-db/issues/10267", "", "TABLE", tbl[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := likeAllRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "LIKE ALL is not supported yet.",
				"https://github.com/yugabyte/yugabyte-db/issues/10697", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := likeRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "LIKE clause not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1129", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := addPrimaryRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			if _, ok := tblPartitions[tbl[3]]; ok {
				reportAddingPrimaryKey(fpath, "TABLE", tbl[3], sqlInfo.formattedStmt)
			}
			primaryCons[tbl[2]] = fpath
		} else if tbl := inheritRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, INHERITANCE_ISSUE_REASON,
				"https://github.com/YugaByte/yugabyte-db/issues/1129", "", "TABLE", tbl[4], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, INHERITANCE_DOC_LINK)
		} else if tbl := withOidsRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "OIDs are not supported for user tables.",
				"https://github.com/yugabyte/yugabyte-db/issues/10273", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := intvlRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "PRIMARY KEY containing column of type 'INTERVAL' not yet supported.",
				"https://github.com/YugaByte/yugabyte-db/issues/1397", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterOfRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE OF not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterSchemaRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE SET SCHEMA not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/3947", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if createSchemaRegex.MatchString(sqlInfo.stmt) {
			reportCase(fpath, "CREATE SCHEMA with elements not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/10865", "", "SCHEMA", "", sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterNotOfRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE NOT OF not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterColumnStatsRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE ALTER column SET STATISTICS not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterColumnStorageRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE ALTER column SET STORAGE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterColumnResetAttributesRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE ALTER column RESET (attribute) not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterConstrRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE ALTER CONSTRAINT not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := setOidsRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE SET WITH OIDS not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[4], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := withoutClusterRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE SET WITHOUT CLUSTER not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterSetRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE SET not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterIdxRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER INDEX SET not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "INDEX", tbl[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterResetRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE RESET not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterOptionsRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if typ := dropAttrRegex.FindStringSubmatch(sqlInfo.stmt); typ != nil {
			reportCase(fpath, "ALTER TYPE DROP ATTRIBUTE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1893", "", "TYPE", typ[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if typ := alterTypeRegex.FindStringSubmatch(sqlInfo.stmt); typ != nil {
			reportCase(fpath, "ALTER TYPE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1893", "", "TYPE", typ[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := alterInhRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE INHERIT not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := valConstrRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "ALTER TABLE VALIDATE CONSTRAINT not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1124", "", "TABLE", tbl[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if spc := alterTblSpcRegex.FindStringSubmatch(sqlInfo.stmt); spc != nil {
			reportCase(fpath, "ALTER TABLESPACE not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1153", "", "TABLESPACE", spc[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if spc := alterViewRegex.FindStringSubmatch(sqlInfo.stmt); spc != nil {
			reportCase(fpath, "ALTER VIEW not supported yet.",
				"https://github.com/YugaByte/yugabyte-db/issues/1131", "", "VIEW", spc[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := cLangRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "LANGUAGE C not supported yet.",
				"https://github.com/yugabyte/yb-voyager/issues/1540", "", "FUNCTION", tbl[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
			summaryMap["FUNCTION"].invalidCount[sqlInfo.objName] = true
		} else if regMatch := partitionColumnsRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			// example1 - CREATE TABLE example1( 	id numeric NOT NULL, 	country_code varchar(3), 	record_type varchar(5), PRIMARY KEY (id,country_code) ) PARTITION BY RANGE (country_code, record_type) ;
			// example2 - CREATE TABLE example2 ( 	id numeric NOT NULL PRIMARY KEY, 	country_code varchar(3), 	record_type varchar(5) ) PARTITION BY RANGE (country_code, record_type) ;
			columnList := utils.CsvStringToSlice(strings.Trim(regMatch[3], " "))
			// example1 - columnList: [id numeric NOT NULL country_code varchar(3) record_type varchar(5) PRIMARY KEY (id country_code)]
			// example2 - columnList: [id numeric NOT NULL PRIMARY KEY country_code varchar(3) record_type varchar(5]
			openBracketSplits := strings.Split(strings.Trim(regMatch[3], " "), "(")
			// example1 -  openBracketSplits: [	id numeric NOT NULL, 	country_code varchar 3), 	record_type varchar 5), 	descriptions varchar 50), 	PRIMARY KEY  id,country_code)]
			stringbeforeLastOpenBracket := ""
			if len(openBracketSplits) > 1 {
				stringbeforeLastOpenBracket = strings.Join(strings.Fields(openBracketSplits[len(openBracketSplits)-2]), " ") //without extra spaces to easily check suffix
			}
			// example1 - stringbeforeLastBracket: 50), PRIMARY KEY
			// example2 - stringbeforeLastBracket: 5), descriptions varchar
			var primaryKeyColumnsList []string
			if strings.HasSuffix(strings.ToLower(stringbeforeLastOpenBracket), ", primary key") { //false for example2
				primaryKeyColumns := strings.Trim(openBracketSplits[len(openBracketSplits)-1], ") ")
				primaryKeyColumnsList = utils.CsvStringToSlice(primaryKeyColumns)
			} else {
				//this case can come by manual intervention
				for _, columnDefinition := range columnList {
					if strings.Contains(strings.ToLower(columnDefinition), "primary key") {
						partsOfColumnDefinition := strings.Split(columnDefinition, " ")
						columnName := partsOfColumnDefinition[0]
						primaryKeyColumnsList = append(primaryKeyColumnsList, columnName)
						break
					}
				}
			}
			partitionColumns := strings.Trim(regMatch[5], `()`)
			partitionColumnsList := utils.CsvStringToSlice(partitionColumns)
			if len(partitionColumnsList) == 1 {
				expressionChk := partitionColumnsList[0]
				if strings.ContainsAny(expressionChk, "()[]{}|/!@$#%^&*-+=") {
					summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
					reportCase(fpath, "Issue with Partition using Expression on a table which cannot contain Primary Key / Unique Key on any column",
						"https://github.com/yugabyte/yb-voyager/issues/698", "Remove the Constriant from the table definition", "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, EXPRESSION_PARTIITON_DOC_LINK)
					continue
				}
			}
			if strings.ToLower(regMatch[4]) == "list" && len(partitionColumnsList) > 1 {
				summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
				reportCase(fpath, `cannot use "list" partition strategy with more than one column`,
					"https://github.com/yugabyte/yb-voyager/issues/699", "Make it a single column partition by list or choose other supported Partitioning methods", "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, LIST_PARTIION_MULTI_COLUMN_DOC_LINK)
				continue
			}
			if len(primaryKeyColumnsList) == 0 { // if non-PK table, then no need to report
				tblPartitions[regMatch[2]] = true
				if filename, ok := primaryCons[regMatch[2]]; ok {
					reportAddingPrimaryKey(filename, "TABLE", tbl[2], sqlInfo.formattedStmt)
				}
				continue
			}
			for _, partitionColumn := range partitionColumnsList {
				if !slices.Contains(primaryKeyColumnsList, partitionColumn) { //partition key not in PK
					summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
					reportCase(fpath, "insufficient columns in the PRIMARY KEY constraint definition in CREATE TABLE",
						"https://github.com/yugabyte/yb-voyager/issues/578", "Add all Partition columns to Primary Key", "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, PARTITION_KEY_NOT_PK_DOC_LINK)
					break
				}
			}
		} else if strings.Contains(strings.ToLower(sqlInfo.stmt), "drop temporary table") {
			filePath := strings.Split(fpath, "/")
			fileName := filePath[len(filePath)-1]
			objType := strings.ToUpper(strings.Split(fileName, ".")[0])
			summaryMap[objType].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, `temporary table is not a supported clause for drop`,
				"https://github.com/yugabyte/yb-voyager/issues/705", `remove "temporary" and change it to "drop table"`, objType, sqlInfo.objName, sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, DROP_TEMP_TABLE_DOC_LINK)
		} else if regMatch := anydataRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "AnyData datatype doesn't have a mapping in YugabyteDB", "https://github.com/yugabyte/yb-voyager/issues/1541", `Remove the column with AnyData datatype or change it to a relevant supported datatype`, "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if regMatch := anydatasetRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "AnyDataSet datatype doesn't have a mapping in YugabyteDB", "https://github.com/yugabyte/yb-voyager/issues/1541", `Remove the column with AnyDataSet datatype or change it to a relevant supported datatype`, "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if regMatch := anyTypeRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "AnyType datatype doesn't have a mapping in YugabyteDB", "https://github.com/yugabyte/yb-voyager/issues/1541", `Remove the column with AnyType datatype or change it to a relevant supported datatype`, "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if regMatch := uriTypeRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			summaryMap["TABLE"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "URIType datatype doesn't have a mapping in YugabyteDB", "https://github.com/yugabyte/yb-voyager/issues/1541", `Remove the column with URIType datatype or change it to a relevant supported datatype`, "TABLE", regMatch[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if regMatch := jsonFuncRegex.FindStringSubmatch(sqlInfo.stmt); regMatch != nil {
			summaryMap[objType].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, "JSON_ARRAYAGG() function is not available in YugabyteDB", "https://github.com/yugabyte/yb-voyager/issues/1542", `Rename the function to YugabyteDB's equivalent JSON_AGG()`, objType, regMatch[3], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		}

	}
}

// check foreign table
func checkForeign(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		//TODO: refactor it later to remove all the unneccessary regexes
		if tbl := primRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "Primary key constraints are not supported on foreign tables.",
				"https://github.com/yugabyte/yugabyte-db/issues/10698", "", "TABLE", tbl[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		} else if tbl := foreignKeyRegex.FindStringSubmatch(sqlInfo.stmt); tbl != nil {
			reportCase(fpath, "Foreign key constraints are not supported on foreign tables.",
				"https://github.com/yugabyte/yugabyte-db/issues/10699", "", "TABLE", tbl[1], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
		}
	}
}

// all other cases to check
func checkRemaining(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		if trig := compoundTrigRegex.FindStringSubmatch(sqlInfo.stmt); trig != nil {
			reportCase(fpath, COMPOUND_TRIGGER_ISSUE_REASON,
				"https://github.com/yugabyte/yb-voyager/issues/1543", "", "TRIGGER", trig[2], sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, "")
			summaryMap["TRIGGER"].invalidCount[sqlInfo.objName] = true
		}
	}

}

// Checks whether the script, fpath, can be migrated to YB
func checker(sqlInfoArr []sqlInfo, fpath string, objType string) {
	if !utils.FileOrFolderExists(fpath) {
		return
	}
	checkViews(sqlInfoArr, fpath)
	checkSql(sqlInfoArr, fpath)
	checkGist(sqlInfoArr, fpath)
	checkGin(sqlInfoArr, fpath)
	checkDDL(sqlInfoArr, fpath, objType)
	checkForeign(sqlInfoArr, fpath)
	checkRemaining(sqlInfoArr, fpath)
	checkStmtsUsingParser(sqlInfoArr, fpath, objType)
}

func checkExtensions(sqlInfoArr []sqlInfo, fpath string) {
	for _, sqlInfo := range sqlInfoArr {
		if sqlInfo.objName != "" && !slices.Contains(supportedExtensionsOnYB, sqlInfo.objName) {
			summaryMap["EXTENSION"].invalidCount[sqlInfo.objName] = true
			reportCase(fpath, UNSUPPORTED_EXTENSION_ISSUE+" Refer to the docs link for the more information on supported extensions.", "https://github.com/yugabyte/yb-voyager/issues/1538", "", "EXTENSION",
				sqlInfo.objName, sqlInfo.formattedStmt, UNSUPPORTED_FEATURES, EXTENSION_DOC_LINK)
		}
		if strings.ToLower(sqlInfo.objName) == "hll" {
			summaryMap["EXTENSION"].details[`'hll' extension is supported in YugabyteDB v2.18 onwards. Please verify this extension as per the target YugabyteDB version.`] = true
		}
	}
}

func invalidSqlComment(line string) int {
	if cmt := unsupportedCommentRegex1.FindStringSubmatch(line); cmt != nil {
		return 1
	} else if cmt := packageSupportCommentRegex.FindStringSubmatch(line); cmt != nil {
		return 2
	} else if cmt := unsupportedCommentRegex2.FindStringSubmatch(line); cmt != nil {
		return 3
	} else if cmt := typeUnsupportedRegex.FindStringSubmatch(line); cmt != nil {
		return 4
	}
	return 0
}

func getCreateObjRegex(objType string) (*regexp.Regexp, int) {
	var createObjRegex *regexp.Regexp
	var objNameIndex int
	//replacing every possible space or new line char with [\s\n]+ in all regexs
	if objType == "MVIEW" {
		createObjRegex = re("CREATE", opt("OR REPLACE"), "MATERIALIZED", "VIEW", capture(ident))
		objNameIndex = 2
	} else if objType == "PACKAGE" {
		createObjRegex = re("CREATE", "SCHEMA", ifNotExists, capture(ident))
		objNameIndex = 2
	} else if objType == "SYNONYM" {
		createObjRegex = re("CREATE", opt("OR REPLACE"), "VIEW", capture(ident))
		objNameIndex = 2
	} else if objType == "INDEX" || objType == "PARTITION_INDEX" || objType == "FTS_INDEX" {
		createObjRegex = re("CREATE", opt("UNIQUE"), "INDEX", ifNotExists, capture(ident))
		objNameIndex = 3
	} else if objType == "TABLE" || objType == "PARTITION" {
		createObjRegex = re("CREATE", opt("OR REPLACE"), "TABLE", ifNotExists, capture(ident))
		objNameIndex = 3
	} else if objType == "FOREIGN TABLE" {
		createObjRegex = re("CREATE", opt("OR REPLACE"), "FOREIGN", "TABLE", ifNotExists, capture(ident))
		objNameIndex = 3
	} else if objType == "OPERATOR" {
		// to include all three types of OPERATOR DDL
		// CREATE OPERATOR / CREATE OPERATOR FAMILY / CREATE OPERATOR CLASS
		createObjRegex = re("CREATE", opt("OR REPLACE"), "OPERATOR", opt("FAMILY"), opt("CLASS"), ifNotExists, capture(ident))
		objNameIndex = 5
	} else { //TODO: check syntaxes for other objects and add more cases if required
		createObjRegex = re("CREATE", opt("OR REPLACE"), objType, ifNotExists, capture(ident))
		objNameIndex = 3
	}

	return createObjRegex, objNameIndex
}

func processCollectedSql(fpath string, stmt string, formattedStmt string, objType string, reportNextSql *int) sqlInfo {
	createObjRegex, objNameIndex := getCreateObjRegex(objType)
	var objName = "" // to extract from sql statement

	//update about sqlStmt in the summary variable for the report generation part
	createObjStmt := createObjRegex.FindStringSubmatch(formattedStmt)
	if createObjStmt != nil {
		objName = createObjStmt[objNameIndex]

		if objType == "PARTITION" || objType == "TABLE" {
			if summaryMap != nil && summaryMap["TABLE"] != nil {
				summaryMap["TABLE"].totalCount += 1
				summaryMap["TABLE"].objSet = append(summaryMap["TABLE"].objSet, objName)
			}
		} else {
			if summaryMap != nil && summaryMap[objType] != nil { //when just createSqlStrArray() is called from someother file, then no summaryMap exists
				summaryMap[objType].totalCount += 1
				summaryMap[objType].objSet = append(summaryMap[objType].objSet, objName)
			}
		}
	} else {
		if objType == "TYPE" {
			//in case of oracle there are some inherited types which can be exported as inherited tables but will be dumped in type.sql
			createObjRegex, objNameIndex = getCreateObjRegex("TABLE")
			createObjStmt = createObjRegex.FindStringSubmatch(formattedStmt)
			if createObjStmt != nil {
				objName = createObjStmt[objNameIndex]
				if summaryMap != nil && summaryMap["TABLE"] != nil {
					summaryMap["TABLE"].totalCount += 1
					summaryMap["TABLE"].objSet = append(summaryMap["TABLE"].objSet, objName)
				}
			}
		}
	}

	if *reportNextSql > 0 && (summaryMap != nil && summaryMap[objType] != nil) {
		reportBasedOnComment(*reportNextSql, fpath, "", "", objName, objType, formattedStmt)
		*reportNextSql = 0 //reset flag
	}

	formattedStmt = strings.TrimRight(formattedStmt, "\n") //removing new line from end

	sqlInfo := sqlInfo{
		objName:       objName,
		stmt:          stmt,
		formattedStmt: formattedStmt,
		fileName:      fpath,
	}
	return sqlInfo
}

func parseSqlFileForObjectType(path string, objType string) []sqlInfo {
	log.Infof("Reading %s DDLs in file %s", objType, path)
	var sqlInfoArr []sqlInfo
	if !utils.FileOrFolderExists(path) {
		return sqlInfoArr
	}
	reportNextSql := 0
	file, err := os.ReadFile(path)
	if err != nil {
		utils.ErrExit("Error while reading %q: %s", path, err)
	}

	lines := strings.Split(string(file), "\n")
	for i := 0; i < len(lines); i++ {
		currLine := lines[i]
		if len(currLine) == 0 {
			continue
		}

		if strings.Contains(strings.TrimLeft(currLine, " "), "--") {
			reportNextSql = invalidSqlComment(currLine)
			continue
		}

		var stmt, formattedStmt string
		if isStartOfCodeBlockSqlStmt(currLine) {
			stmt, formattedStmt = collectSqlStmtContainingCode(lines, &i)
		} else {
			stmt, formattedStmt = collectSqlStmt(lines, &i)
		}
		sqlInfo := processCollectedSql(path, stmt, formattedStmt, objType, &reportNextSql)
		sqlInfoArr = append(sqlInfoArr, sqlInfo)
	}

	return sqlInfoArr
}

var reCreateProc, _ = getCreateObjRegex("PROCEDURE")
var reCreateFunc, _ = getCreateObjRegex("FUNCTION")
var reCreateTrigger, _ = getCreateObjRegex("TRIGGER")

// returns true when sql stmt is a CREATE statement for TRIGGER, FUNCTION, PROCEDURE
func isStartOfCodeBlockSqlStmt(line string) bool {
	return reCreateProc.MatchString(line) || reCreateFunc.MatchString(line) || reCreateTrigger.MatchString(line)
}

const (
	CODE_BLOCK_NOT_STARTED = 0
	CODE_BLOCK_STARTED     = 1
	CODE_BLOCK_COMPLETED   = 2
)

func collectSqlStmtContainingCode(lines []string, i *int) (string, string) {
	dollarQuoteFlag := CODE_BLOCK_NOT_STARTED
	// Delimiter to outermost Code Block if nested Code Blocks present
	codeBlockDelimiter := ""

	stmt := ""
	formattedStmt := ""

sqlParsingLoop:
	for ; *i < len(lines); *i++ {
		currLine := strings.TrimRight(lines[*i], " ")
		if len(currLine) == 0 {
			continue
		}

		stmt += currLine + " "
		formattedStmt += currLine + "\n"

		// Assuming that both the dollar quote strings will not be in same line
		switch dollarQuoteFlag {
		case CODE_BLOCK_NOT_STARTED:
			if isEndOfSqlStmt(currLine) { // in case, there is no body part or body part is in single line
				break sqlParsingLoop
			} else if matches := dollarQuoteRegex.FindStringSubmatch(currLine); matches != nil {
				dollarQuoteFlag = 1 //denotes start of the code/body part
				codeBlockDelimiter = matches[0]
			}
		case CODE_BLOCK_STARTED:
			if strings.Contains(currLine, codeBlockDelimiter) {
				dollarQuoteFlag = 2 //denotes end of code/body part
				if isEndOfSqlStmt(currLine) {
					break sqlParsingLoop
				}
			}
		case CODE_BLOCK_COMPLETED:
			if isEndOfSqlStmt(currLine) {
				break sqlParsingLoop
			}
		}
	}

	return stmt, formattedStmt
}

func collectSqlStmt(lines []string, i *int) (string, string) {
	stmt := ""
	formattedStmt := ""
	for ; *i < len(lines); *i++ {
		currLine := strings.TrimRight(lines[*i], " ")
		if len(currLine) == 0 {
			continue
		}

		stmt += currLine + " "
		formattedStmt += currLine + "\n"

		if isEndOfSqlStmt(currLine) {
			break
		}
	}
	return stmt, formattedStmt
}

func isEndOfSqlStmt(line string) bool {
	/*	checking for string with ending with `;`
		Also, cover cases like comment at the end after `;`
		example: "CREATE TABLE t1 (c1 int); -- table t1" */
	line = strings.TrimRight(line, " ")
	if line[len(line)-1] == ';' {
		return true
	}

	cmtStartIdx := strings.LastIndex(line, "--")
	if cmtStartIdx != -1 {
		line = line[0:cmtStartIdx] // ignore comment
		line = strings.TrimRight(line, " ")
	}
	return line[len(line)-1] == ';'
}

func initializeSummaryMap() {
	log.Infof("initializing report summary map")
	for _, objType := range sourceObjList {
		summaryMap[objType] = &summaryInfo{
			invalidCount: make(map[string]bool),
			objSet:       make([]string, 0),
			details:      make(map[string]bool),
		}

		//executes only in case of oracle
		if objType == "PACKAGE" {
			summaryMap[objType].details["Packages in oracle are exported as schema, please review and edit them(if needed) to match your requirements"] = true
		} else if objType == "SYNONYM" {
			summaryMap[objType].details["Synonyms in oracle are exported as view, please review and edit them(if needed) to match your requirements"] = true
		}
	}

}

//go:embed templates/schema_analysis_report.html
var schemaAnalysisHtmlTmpl string

//go:embed templates/schema_analysis_report.txt
var schemaAnalysisTxtTmpl string

func applyTemplate(Report utils.SchemaReport, templateString string) (string, error) {
	tmpl, err := template.New("schema_analysis_report").Funcs(funcMap).Parse(templateString)
	if err != nil {
		return "", fmt.Errorf("failed to parse template file: %w", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, Report)
	if err != nil {
		return "", fmt.Errorf("failed to execute parse template file: %w", err)
	}

	return buf.String(), nil
}

var funcMap = template.FuncMap{
	"join": func(arr []string, sep string) string {
		return strings.Join(arr, sep)
	},
	"sub": func(a int, b int) int {
		return a - b
	},
	"add": func(a int, b int) int {
		return a + b
	},
	"sumDbObjects": func(dbObjects []utils.DBObject, field string) int {
		total := 0
		for _, obj := range dbObjects {
			switch field {
			case "TotalCount":
				total += obj.TotalCount
			case "InvalidCount":
				total += obj.InvalidCount
			case "ValidCount":
				total += obj.TotalCount - obj.InvalidCount
			}
		}
		return total
	},
}

// add info to the 'reportStruct' variable and return
func analyzeSchemaInternal(sourceDBConf *srcdb.Source) utils.SchemaReport {
	/*
		NOTE: Don't create local var with name 'schemaAnalysisReport' since global one
		is used across all the internal functions called by analyzeSchemaInternal()
	*/
	sourceDBType = sourceDBConf.DBType
	schemaAnalysisReport = utils.SchemaReport{}
	sourceObjList = utils.GetSchemaObjectList(sourceDBConf.DBType)
	initializeSummaryMap()
	for _, objType := range sourceObjList {
		var sqlInfoArr []sqlInfo
		filePath := utils.GetObjectFilePath(schemaDir, objType)
		if objType != "INDEX" {
			sqlInfoArr = parseSqlFileForObjectType(filePath, objType)
		} else {
			sqlInfoArr = parseSqlFileForObjectType(filePath, objType)
			otherFPaths := utils.GetObjectFilePath(schemaDir, "PARTITION_INDEX")
			sqlInfoArr = append(sqlInfoArr, parseSqlFileForObjectType(otherFPaths, "PARTITION_INDEX")...)
			otherFPaths = utils.GetObjectFilePath(schemaDir, "FTS_INDEX")
			sqlInfoArr = append(sqlInfoArr, parseSqlFileForObjectType(otherFPaths, "FTS_INDEX")...)
		}
		if objType == "EXTENSION" {
			checkExtensions(sqlInfoArr, filePath)
		}
		if objType == "FOREIGN TABLE" {
			checkForeignTable(sqlInfoArr, filePath)
		}
		checker(sqlInfoArr, filePath, objType)

		if objType == "CONVERSION" {
			checkConversions(sqlInfoArr, filePath)
		}
	}

	schemaAnalysisReport.SchemaSummary = reportSchemaSummary(sourceDBConf)
	return schemaAnalysisReport
}

func checkConversions(sqlInfoArr []sqlInfo, filePath string) {
	for _, sqlStmtInfo := range sqlInfoArr {
		parseTree, err := pg_query.Parse(sqlStmtInfo.stmt)
		if err != nil {
			utils.ErrExit("failed to parse the stmt %v: %v", sqlStmtInfo.stmt, err)
		}

		createConvNode, ok := parseTree.Stmts[0].Stmt.Node.(*pg_query.Node_CreateConversionStmt)
		if ok {
			createConvStmt := createConvNode.CreateConversionStmt
			//Conversion name here is a list of items which are '.' separated
			//so 0th and 1st indexes are Schema and conv name respectively
			nameList := createConvStmt.GetConversionName()
			convName := nameList[0].GetString_().Sval
			if len(nameList) > 1 {
				convName = fmt.Sprintf("%s.%s", convName, nameList[1].GetString_().Sval)
			}
			reportCase(filePath, CONVERSION_ISSUE_REASON, "https://github.com/yugabyte/yugabyte-db/issues/10866",
				"Remove it from the exported schema", "CONVERSION", convName, sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, CREATE_CONVERSION_DOC_LINK)
		} else {
			//pg_query doesn't seem to have a Node type of AlterConversionStmt so using regex for now
			if stmt := alterConvRegex.FindStringSubmatch(sqlStmtInfo.stmt); stmt != nil {
				reportCase(filePath, "ALTER CONVERSION is not supported yet", "https://github.com/YugaByte/yugabyte-db/issues/10866",
					"Remove it from the exported schema", "CONVERSION", stmt[1], sqlStmtInfo.formattedStmt, UNSUPPORTED_FEATURES, CREATE_CONVERSION_DOC_LINK)
			}
		}

	}
}

func analyzeSchema() {
	err := retrieveMigrationUUID()
	if err != nil {
		utils.ErrExit("failed to get migration UUID: %w", err)
	}
	reportFile := "schema_analysis_report." + analyzeSchemaReportFormat

	schemaAnalysisStartedEvent := createSchemaAnalysisStartedEvent()
	controlPlane.SchemaAnalysisStarted(&schemaAnalysisStartedEvent)

	reportPath := filepath.Join(exportDir, "reports", reportFile)

	if !schemaIsExported() {
		utils.ErrExit("run export schema before running analyze-schema")
	}

	msr, err := metaDB.GetMigrationStatusRecord()
	if err != nil {
		utils.ErrExit("analyze schema : load migration status record: %s", err)
	}
	analyzeSchemaInternal(msr.SourceDBConf)

	var finalReport string

	switch analyzeSchemaReportFormat {
	case "html":
		if msr.SourceDBConf.DBType == POSTGRESQL {
			// marking this as empty to not display this in html report for PG
			schemaAnalysisReport.SchemaSummary.SchemaNames = []string{}
		}
		finalReport, err = applyTemplate(schemaAnalysisReport, schemaAnalysisHtmlTmpl)
		if err != nil {
			utils.ErrExit("failed to apply template for html schema analysis report: %v", err)
		}
	case "json":
		jsonReportBytes, err := json.MarshalIndent(schemaAnalysisReport, "", "    ")
		if err != nil {
			utils.ErrExit("failed to marshal the report struct into json schema analysis report: %v", err)
		}
		finalReport = string(jsonReportBytes)
	case "txt":
		finalReport, err = applyTemplate(schemaAnalysisReport, schemaAnalysisTxtTmpl)
		if err != nil {
			utils.ErrExit("failed to apply template for txt schema analysis report: %v", err)
		}
	case "xml":
		xmlReportBytes, err := xml.MarshalIndent(schemaAnalysisReport, "", "\t")
		if err != nil {
			utils.ErrExit("failed to marshal the report struct into xml schema analysis report: %v", err)
		}
		finalReport = string(xmlReportBytes)
	default:
		panic(fmt.Sprintf("invalid report format: %q", analyzeSchemaReportFormat))
	}

	//check & inform if file already exists
	if utils.FileOrFolderExists(reportPath) {
		fmt.Printf("\n%s already exists, overwriting it with a new generated report\n", reportFile)
	}

	file, err := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		utils.ErrExit("Error while opening %q: %s", reportPath, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			log.Errorf("Error while closing file %s: %v", reportPath, err)
		}
	}()

	_, err = file.WriteString(finalReport)
	if err != nil {
		utils.ErrExit("failed to write report to %q: %s", reportPath, err)
	}
	fmt.Printf("-- find schema analysis report at: %s\n", reportPath)

	packAndSendAnalyzeSchemaPayload(COMPLETE)

	schemaAnalysisReport := createSchemaAnalysisIterationCompletedEvent(schemaAnalysisReport)
	controlPlane.SchemaAnalysisIterationCompleted(&schemaAnalysisReport)
}

func packAndSendAnalyzeSchemaPayload(status string) {
	if !shouldSendCallhome() {
		return
	}
	payload := createCallhomePayload()

	payload.MigrationPhase = ANALYZE_PHASE
	var callhomeIssues []utils.Issue
	for _, issue := range schemaAnalysisReport.Issues {
		issue.SqlStatement = "" // Obfuscate sensitive information before sending to callhome cluster
		callhomeIssues = append(callhomeIssues, issue)
	}
	analyzePayload := callhome.AnalyzePhasePayload{
		Issues:          callhome.MarshalledJsonString(callhomeIssues),
		DatabaseObjects: callhome.MarshalledJsonString(schemaAnalysisReport.SchemaSummary.DBObjects),
	}
	payload.PhasePayload = callhome.MarshalledJsonString(analyzePayload)
	payload.Status = status

	err := callhome.SendPayload(&payload)
	if err == nil && (status == COMPLETE || status == ERROR) {
		callHomeErrorOrCompletePayloadSent = true
	}
}

var analyzeSchemaCmd = &cobra.Command{
	Use: "analyze-schema",
	Short: "Analyze converted source database schema and generate a report about YB incompatible constructs.\n" +
		"For more details and examples, visit https://docs.yugabyte.com/preview/yugabyte-voyager/reference/schema-migration/analyze-schema/",
	Long: ``,
	PreRun: func(cmd *cobra.Command, args []string) {
		validOutputFormats := []string{"html", "json", "txt", "xml"}
		validateReportOutputFormat(validOutputFormats, analyzeSchemaReportFormat)
	},

	Run: func(cmd *cobra.Command, args []string) {
		analyzeSchema()
	},
}

func init() {
	rootCmd.AddCommand(analyzeSchemaCmd)
	registerCommonGlobalFlags(analyzeSchemaCmd)
	analyzeSchemaCmd.PersistentFlags().StringVar(&analyzeSchemaReportFormat, "output-format", "txt",
		"format in which report will be generated: (html, txt, json, xml)")
}

func validateReportOutputFormat(validOutputFormats []string, format string) {
	format = strings.ToLower(format)

	for i := 0; i < len(validOutputFormats); i++ {
		if format == validOutputFormats[i] {
			return
		}
	}
	utils.ErrExit("Error: Invalid output format: %s. Supported formats are %v", format, validOutputFormats)
}

func schemaIsAnalyzed() bool {
	path := filepath.Join(exportDir, "reports", "schema_analysis_report.*")
	return utils.FileOrFolderExistsWithGlobPattern(path)
}

func createSchemaAnalysisStartedEvent() cp.SchemaAnalysisStartedEvent {
	result := cp.SchemaAnalysisStartedEvent{}
	initBaseSourceEvent(&result.BaseEvent, "ANALYZE SCHEMA")
	return result
}

func createSchemaAnalysisIterationCompletedEvent(report utils.SchemaReport) cp.SchemaAnalysisIterationCompletedEvent {
	result := cp.SchemaAnalysisIterationCompletedEvent{}
	initBaseSourceEvent(&result.BaseEvent, "ANALYZE SCHEMA")
	result.AnalysisReport = report
	return result
}
