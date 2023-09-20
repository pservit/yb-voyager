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
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/exp/slices"

	"github.com/yugabyte/yb-voyager/yb-voyager/src/tgtdb"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/utils"
)

var sourceDBType string
var enableOrafce bool
var importType string

// tconf struct will be populated by CLI arguments parsing
var tconf tgtdb.TargetConf

var tdb tgtdb.TargetDB

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import schema and data from compatible source database(Oracle, MySQL, PostgreSQL)",
	Long:  `Import has various sub-commands i.e. import schema and import data to import into YugabyteDB from various compatible source databases(Oracle, MySQL, PostgreSQL).`,
}

func init() {
	rootCmd.AddCommand(importCmd)
}

// If any changes are made to this function, verify if the change is also needed for importDataFileCommand.go
func validateImportFlags(cmd *cobra.Command, importerRole string) {
	validateExportDirFlag()
	checkOrSetDefaultTargetSSLMode()
	validateTargetPortRange()
	if tconf.TableList != "" && tconf.ExcludeTableList != "" {
		utils.ErrExit("Error: Only one of --table-list and --exclude-table-list are allowed")
	}
	validateTableListFlag(tconf.TableList, "table-list")
	validateTableListFlag(tconf.ExcludeTableList, "exclude-table-list")
	if tconf.ImportObjects != "" && tconf.ExcludeImportObjects != "" {
		utils.ErrExit("Error: Only one of --object-list and --exclude-object-list are allowed")
	}
	validateImportObjectsFlag(tconf.ImportObjects, "object-list")
	validateImportObjectsFlag(tconf.ExcludeImportObjects, "exclude-object-list")
	validateTargetSchemaFlag()
	// For beta2.0 release (and onwards until further notice)
	if tconf.DisableTransactionalWrites {
		fmt.Println("WARNING: The --disable-transactional-writes feature is in the experimental phase, not for production use case.")
	}
	validateBatchSizeFlag(batchSize)
	switch importerRole {
	case TARGET_DB_IMPORTER_ROLE:
		getTargetPassword(cmd)
	case FF_DB_IMPORTER_ROLE:
		getFallForwardDBPassword(cmd)
	}
}

func registerCommonImportFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&startClean, "start-clean", false,
		"import schema: delete all existing schema objects \nimport data / import data file: starts a fresh import of data or incremental data load")

	cmd.Flags().BoolVar(&tconf.VerboseMode, "verbose", false,
		"verbose mode for some extra details during execution of command")

	cmd.Flags().BoolVar(&tconf.ContinueOnError, "continue-on-error", false,
		"If set, this flag will ignore errors and continue with the import")
}

func registerTargetDBConnFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&tconf.Host, "target-db-host", "127.0.0.1",
		"host on which the YugabyteDB server is running")

	cmd.Flags().IntVar(&tconf.Port, "target-db-port", -1,
		"port on which the YugabyteDB YSQL API is running (Default: 5433)")

	cmd.Flags().StringVar(&tconf.User, "target-db-user", "",
		"username with which to connect to the target YugabyteDB server")
	cmd.MarkFlagRequired("target-db-user")

	cmd.Flags().StringVar(&tconf.Password, "target-db-password", "",
		"password with which to connect to the target YugabyteDB server")

	cmd.Flags().StringVar(&tconf.DBName, "target-db-name", "",
		"name of the database on the target YugabyteDB server on which import needs to be done")

	cmd.Flags().StringVar(&tconf.DBSid, "target-db-sid", "",
		"[For Oracle Only] Oracle System Identifier (SID) that you wish to use while importing data to Oracle instances")

	cmd.Flags().StringVar(&tconf.Schema, "target-db-schema", "",
		"target schema name in YugabyteDB (Note: works only for source as Oracle and MySQL, in case of PostgreSQL you can ALTER schema name post import)")

	// TODO: SSL related more args might come. Need to explore SSL part completely.
	cmd.Flags().StringVar(&tconf.SSLCertPath, "target-ssl-cert", "",
		"provide target SSL Certificate Path")

	cmd.Flags().StringVar(&tconf.SSLMode, "target-ssl-mode", "prefer",
		"specify the target SSL mode out of - disable, allow, prefer, require, verify-ca, verify-full")

	cmd.Flags().StringVar(&tconf.SSLKey, "target-ssl-key", "",
		"target SSL Key Path")

	cmd.Flags().StringVar(&tconf.SSLRootCert, "target-ssl-root-cert", "",
		"target SSL Root Certificate Path")

	cmd.Flags().StringVar(&tconf.SSLCRL, "target-ssl-crl", "",
		"target SSL Root Certificate Revocation List (CRL)")
}

func registerFFDBAsTargetConnFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&tconf.Host, "ff-db-host", "127.0.0.1",
		"host on which the Fall-forward DB server is running")

	cmd.Flags().IntVar(&tconf.Port, "ff-db-port", -1,
		"port on which the Fall-forward DB server is running Default: ORACLE(1521)")

	cmd.Flags().StringVar(&tconf.User, "ff-db-user", "",
		"username with which to connect to the Fall-forward DB server")
	cmd.MarkFlagRequired("ff-db-user")

	cmd.Flags().StringVar(&tconf.Password, "ff-db-password", "",
		"password with which to connect to the Fall-forward DB server")

	cmd.Flags().StringVar(&tconf.DBName, "ff-db-name", "",
		"name of the database on the Fall-forward DB server on which import needs to be done")

	cmd.Flags().StringVar(&tconf.DBSid, "ff-db-sid", "",
		"[For Oracle Only] Oracle System Identifier (SID) that you wish to use while importing data to Oracle instances")

	cmd.Flags().StringVar(&tconf.OracleHome, "oracle-home", "",
		"[For Oracle Only] Path to set $ORACLE_HOME environment variable. tnsnames.ora is found in $ORACLE_HOME/network/admin")

	cmd.Flags().StringVar(&tconf.TNSAlias, "oracle-tns-alias", "",
		"[For Oracle Only] Name of TNS Alias you wish to use to connect to Oracle instance. Refer to documentation to learn more about configuring tnsnames.ora and aliases")

	cmd.Flags().StringVar(&tconf.Schema, "ff-db-schema", "",
		"schema name in Fall-forward DB") // TODO: add back note after we suppport PG/Mysql - `(Note: works only for source as Oracle and MySQL, in case of PostgreSQL you can ALTER schema name post import)`

	// TODO: SSL related more args might come. Need to explore SSL part completely.
	cmd.Flags().StringVar(&tconf.SSLCertPath, "ff-ssl-cert", "",
		"provide Fall-forward DB SSL Certificate Path")

	cmd.Flags().StringVar(&tconf.SSLMode, "ff-ssl-mode", "prefer",
		"specify the Fall-forward DB SSL mode out of - disable, allow, prefer, require, verify-ca, verify-full")

	cmd.Flags().StringVar(&tconf.SSLKey, "ff-ssl-key", "",
		"Fall-forward DB SSL Key Path")

	cmd.Flags().StringVar(&tconf.SSLRootCert, "ff-ssl-root-cert", "",
		"Fall-forward DB SSL Root Certificate Path")

	cmd.Flags().StringVar(&tconf.SSLCRL, "ff-ssl-crl", "",
		"Fall-forward DB SSL Root Certificate Revocation List (CRL)")
}

func registerImportDataFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&disablePb, "disable-pb", false,
		"true - to disable progress bar during data import and stats printing during streaming phase (default false)")
	cmd.Flags().StringVar(&tconf.ExcludeTableList, "exclude-table-list", "",
		"list of tables to exclude while importing data (ignored if --table-list is used)")
	cmd.Flags().StringVar(&tconf.TableList, "table-list", "",
		"list of tables to import data")
	defaultbatchSize := int64(DEFAULT_BATCH_SIZE_YUGABYTEDB)
	if cmd.CommandPath() == "yb-voyager fall-forward setup" {
		defaultbatchSize = int64(DEFAULT_BATCH_SIZE_ORACLE)
	}
	cmd.Flags().Int64Var(&batchSize, "batch-size", defaultbatchSize,
		"maximum number of rows in each batch generated during import.")
	defaultParallelismMsg := "By default, voyager will try if it can determine the total number of cores N and use N/2 as parallel jobs. " +
		"Otherwise, it fall back to using twice the number of nodes in the cluster"
	if cmd.CommandPath() == "yb-voyager fall-forward setup" {
		defaultParallelismMsg = ""
	}
	cmd.Flags().IntVar(&tconf.Parallelism, "parallel-jobs", -1,
		"number of parallel copy command jobs to target database. "+defaultParallelismMsg)
	cmd.Flags().BoolVar(&tconf.EnableUpsert, "enable-upsert", true,
		"true - to enable UPSERT mode on target tables\n"+
			"false - to disable UPSERT mode on target tables")
	cmd.Flags().BoolVar(&tconf.UsePublicIP, "use-public-ip", false,
		"true - to use the public IPs of the nodes to distribute --parallel-jobs uniformly for data import (default false)\n"+
			"Note: you might need to configure database to have public_ip available by setting server-broadcast-addresses.\n"+
			"Refer: https://docs.yugabyte.com/latest/reference/configuration/yb-tserver/#server-broadcast-addresses")
	cmd.Flags().StringVar(&tconf.TargetEndpoints, "target-endpoints", "",
		"comma separated list of node's endpoint to use for parallel import of data(default is to use all the nodes in the cluster).\n"+
			"For example: \"host1:port1,host2:port2\" or \"host1,host2\"\n"+
			"Note: use-public-ip flag will be ignored if this is used.")
	// flag existence depends on fix of this gh issue: https://github.com/yugabyte/yugabyte-db/issues/12464
	cmd.Flags().BoolVar(&tconf.DisableTransactionalWrites, "disable-transactional-writes", false,
		"true - to disable transactional writes in tables for faster data ingestion (default false)\n"+
			"(Note: this is a interim flag until the issues related to 'yb_disable_transactional_writes' session variable are fixed. Refer: https://github.com/yugabyte/yugabyte-db/issues/12464)")
	// Hidden for beta2.0 release (and onwards until further notice).
	cmd.Flags().MarkHidden("disable-transactional-writes")

	cmd.Flags().BoolVar(&truncateSplits, "truncate-splits", true,
		"true - to truncate splits after importing\n"+
			"false - to not truncate splits after importing (required for debugging)")
	cmd.Flags().MarkHidden("truncate-splits")
}

func registerImportSchemaFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&tconf.ImportObjects, "object-list", "",
		"list of schema object types to include while importing schema")
	cmd.Flags().StringVar(&tconf.ExcludeImportObjects, "exclude-object-list", "",
		"list of schema object types to exclude while importing schema (ignored if --object-list is used)")
	cmd.Flags().BoolVar(&importObjectsInStraightOrder, "straight-order", false,
		"If set, objects will be imported in the order specified with the --object-list flag (default false)")
	cmd.Flags().BoolVar(&flagPostImportData, "post-import-data", false,
		"If set, creates indexes, foreign-keys, and triggers in target db")
	cmd.Flags().BoolVar(&tconf.IgnoreIfExists, "ignore-exist", false,
		"true - to ignore errors if object already exists\n"+
			"false - throw those errors to the standard output (default false)")
	cmd.Flags().BoolVar(&flagRefreshMViews, "refresh-mviews", false,
		"If set, refreshes the materialised views on target during post import data phase (default false)")
	cmd.Flags().BoolVar(&enableOrafce, "enable-orafce", true,
		"true - to enable Orafce extension on target(if source db type is Oracle)")
}

func validateTargetPortRange() {
	if tconf.Port == -1 {
		if tconf.TargetDBType == ORACLE {
			tconf.Port = ORACLE_DEFAULT_PORT
		} else if tconf.TargetDBType == YUGABYTEDB {
			tconf.Port = YUGABYTEDB_YSQL_DEFAULT_PORT
		}
		return
	}

	if tconf.Port < 0 || tconf.Port > 65535 {
		utils.ErrExit("Invalid port number %d. Valid range is 0-65535", tconf.Port)
	}
}

func validateTargetSchemaFlag() {
	if tconf.Schema == "" {
		if tconf.TargetDBType == YUGABYTEDB {
			tconf.Schema = YUGABYTEDB_DEFAULT_SCHEMA
		} else if tconf.TargetDBType == ORACLE {
			tconf.Schema = tconf.User
		}
		return
	}
	if tconf.Schema != YUGABYTEDB_DEFAULT_SCHEMA && sourceDBType == "postgresql" {
		utils.ErrExit("Error: --target-db-schema flag is not valid for export from 'postgresql' db type")
	}
}

func getTargetPassword(cmd *cobra.Command) {
	var err error
	tconf.Password, err = getPassword(cmd, "target-db-password", "TARGET_DB_PASSWORD")
	if err != nil {
		utils.ErrExit("error in getting target-db-password: %v", err)
	}
}

func getFallForwardDBPassword(cmd *cobra.Command) {
	var err error
	tconf.Password, err = getPassword(cmd, "ff-db-password", "FF_DB_PASSWORD")
	if err != nil {
		utils.ErrExit("error while getting ff-db-password: %w", err)
	}
}

func validateImportObjectsFlag(importObjectsString string, flagName string) {
	if importObjectsString == "" {
		return
	}
	//We cannot access sourceDBType variable at this point, but exportDir has been validated
	availableObjects := utils.GetSchemaObjectList(ExtractMetaInfo(exportDir).SourceDBType)
	objectList := utils.CsvStringToSlice(importObjectsString)
	for _, object := range objectList {
		if !slices.Contains(availableObjects, strings.ToUpper(object)) {
			utils.ErrExit("Error: Invalid object type '%v' specified wtih --%s flag. Supported object types are: %v", object, flagName, availableObjects)
		}
	}
}

func checkOrSetDefaultTargetSSLMode() {
	if tconf.SSLMode == "" {
		tconf.SSLMode = "prefer"
	} else if tconf.SSLMode != "disable" && tconf.SSLMode != "prefer" && tconf.SSLMode != "require" && tconf.SSLMode != "verify-ca" && tconf.SSLMode != "verify-full" {
		utils.ErrExit("Invalid sslmode %q. Required one of [disable, allow, prefer, require, verify-ca, verify-full]", tconf.SSLMode)
	}
}

func validateBatchSizeFlag(numLinesInASplit int64) {
	if batchSize == -1 {
		if tconf.TargetDBType == ORACLE {
			batchSize = DEFAULT_BATCH_SIZE_ORACLE
		} else {
			batchSize = DEFAULT_BATCH_SIZE_YUGABYTEDB
		}
		return
	}

	var defaultBatchSize int64
	if tconf.TargetDBType == ORACLE {
		defaultBatchSize = DEFAULT_BATCH_SIZE_ORACLE
	} else {
		defaultBatchSize = DEFAULT_BATCH_SIZE_YUGABYTEDB
	}

	if numLinesInASplit > defaultBatchSize {
		utils.ErrExit("Error: Invalid batch size %v. The batch size cannot be greater than %v", numLinesInASplit, defaultBatchSize)
	}
}

func validateFFDBSchemaFlag() {
	if tconf.Schema == "" {
		utils.ErrExit("Error: --ff-db-schema flag is mandatory for fall-forward setup")
	}
}
