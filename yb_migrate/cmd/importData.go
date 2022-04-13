/*
Copyright (c) YugaByte, Inc.

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
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/yugabyte/ybm/yb_migrate/src/fwk"
	"github.com/yugabyte/ybm/yb_migrate/src/migration"
	"github.com/yugabyte/ybm/yb_migrate/src/utils"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/vbauerster/mpb/v7"

	"github.com/jackc/pgx/v4"
	"github.com/tevino/abool/v2"
)

var splitFileChannelSize = SPLIT_FILE_CHANNEL_SIZE
var metaInfoDir = META_INFO_DIR_NAME
var importLockFile = fmt.Sprintf("%s/%s/data/.importLock", exportDir, metaInfoDir)
var numLinesInASplit = int64(0)
var parallelImportJobs = 0
var Done = abool.New()
var GenerateSplitsDone = abool.New()

var tablesProgressMetadata map[string]*utils.TableProgressMetadata

type ProgressContainer struct {
	mu        sync.Mutex
	container *mpb.Progress
}

var importProgressContainer ProgressContainer
var importTables []string
var allTables []string
var usePublicIp bool
var targetEndpoints string
var copyTableFromCommands = make(map[string]string)

type ExportTool int

const (
	Ora2Pg = iota
	YsqlDump
	PgDump
)

var importDataCmd = &cobra.Command{
	Use:   "data",
	Short: "This command imports data into YugabyteDB database",
	Long:  `This command will import the data exported from the source database into YugabyteDB database.`,

	PreRun: func(cmd *cobra.Command, args []string) {
		validateImportFlags(cmd)
	},

	Run: func(cmd *cobra.Command, args []string) {
		target.ImportMode = true
		importData()
	},
}

func getYBServers() []*utils.Target {
	var targets []*utils.Target

	if targetEndpoints != "" {
		msg := fmt.Sprintf("given yb-servers for import data: %q\n", targetEndpoints)
		utils.PrintIfTrue(msg, target.VerboseMode)
		log.Infof(msg)

		ybServers := utils.CsvStringToSlice(targetEndpoints)
		for _, ybServer := range ybServers {
			clone := cloneTarget(&target)

			if strings.Contains(ybServer, ":") {
				clone.Host = strings.Split(ybServer, ":")[0]
				var err error
				clone.Port, err = strconv.Atoi(strings.Split(ybServer, ":")[1])

				if err != nil {
					utils.ErrExit("error in parsing useYbServers flag: %v", err)
				}
			} else {
				clone.Host = ybServer
			}

			clone.Uri = getCloneConnectionUri(clone)
			log.Infof("using yb server for import data: %+v", clone)
			targets = append(targets, clone)
		}
	} else {
		url := getTargetConnectionUri(&target)
		conn, err := pgx.Connect(context.Background(), url)
		if err != nil {
			utils.ErrExit("Unable to connect to database: %v", err)
		}
		defer conn.Close(context.Background())

		rows, err := conn.Query(context.Background(), GET_SERVERS_QUERY)
		if err != nil {
			utils.ErrExit("error in query rows from yb_servers(): %v", err)
		}
		defer rows.Close()

		var hostPorts []string
		for rows.Next() {
			clone := cloneTarget(&target)
			var host, nodeType, cloud, region, zone, public_ip string
			var port, num_conns int
			if err := rows.Scan(&host, &port, &num_conns,
				&nodeType, &cloud, &region, &zone, &public_ip); err != nil {
				utils.ErrExit("error in scanning rows of yb_servers(): %v", err)
			}
			if usePublicIp {
				if public_ip == "" {
					log.Infof("no public_ip available for host: %s", host)
					continue
				}
				clone.Host = public_ip
			} else {
				clone.Host = host
			}

			clone.Port = port
			clone.Uri = getCloneConnectionUri(clone)
			targets = append(targets, clone)

			hostPorts = append(hostPorts, fmt.Sprintf("%s:%v", host, port))
		}
		log.Infof("Target DB nodes: %s", strings.Join(hostPorts, ","))
	}

	testYbServers(targets)
	return targets
}

func testYbServers(targets []*utils.Target) {
	if len(targets) == 0 {
		utils.ErrExit("no yb servers available/given for data import")
	}
	for _, target := range targets {
		log.Infof("testing server: %s\n", spew.Sdump(target))
		conn, err := pgx.Connect(context.Background(), target.Uri)
		if err != nil {
			utils.ErrExit("error while testing yb servers: %v", err)
		}
		conn.Close(context.Background())
	}
	log.Infof("all target servers are accessible")
}

func cloneTarget(t *utils.Target) *utils.Target {
	var clone utils.Target
	clone = *t
	return &clone
}

func getCloneConnectionUri(clone *utils.Target) string {
	var cloneConnectionUri string
	if clone.Uri == "" {
		//fallback to constructing the URI from individual parameters. If URI was not set for target, then its other necessary parameters must be non-empty (or default values)
		cloneConnectionUri = getTargetConnectionUri(clone)
	} else {
		targetConnectionUri, err := url.Parse(clone.Uri)
		if err == nil {
			targetConnectionUri.Host = fmt.Sprintf("%s:%d", clone.Host, clone.Port)
			cloneConnectionUri = fmt.Sprint(targetConnectionUri)
		} else {
			panic(err)
		}
	}
	return cloneConnectionUri
}

func getTargetConnectionUri(targetStruct *utils.Target) string {
	if len(targetStruct.Uri) != 0 {
		return targetStruct.Uri
	} else {
		uri := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?%s",
			targetStruct.User, targetStruct.Password, targetStruct.Host, targetStruct.Port, targetStruct.DBName, generateSSLQueryStringIfNotExists(targetStruct))
		targetStruct.Uri = uri
		return uri
	}
}

func importData() {
	// TODO: Add later
	// acquireImportLock()
	// defer os.Remove(importLockFile)
	log.Infof("import data command initiated for DB %q", target.DBName)
	targets := getYBServers()

	var parallelism = parallelImportJobs
	if parallelism == -1 {
		parallelism = len(targets)
	}
	log.Infof("parallelism=%v", parallelism)
	if target.VerboseMode {
		fmt.Printf("Number of parallel imports jobs at a time: %d\n", parallelism)
	}

	if parallelism > SPLIT_FILE_CHANNEL_SIZE {
		splitFileChannelSize = parallelism + 1
	}
	splitFilesChannel := make(chan *fwk.SplitFileImportTask, splitFileChannelSize)
	targetServerChannel := make(chan *utils.Target, 1)

	go roundRobinTargets(targets, targetServerChannel)
	generateSmallerSplits(splitFilesChannel)
	go doImport(splitFilesChannel, parallelism, targetServerChannel)
	checkForDone()

	time.Sleep(time.Second * 2)
	executePostImportDataSqls()
	fmt.Printf("\nexiting...\n")
}

func checkForDone() {
	for Done.IsNotSet() {
		if GenerateSplitsDone.IsSet() {
			// InProgress Pattern
			inProgressPattern := fmt.Sprintf("%s/%s/data/*.P", exportDir, metaInfoDir)
			m1, err := filepath.Glob(inProgressPattern)
			if err != nil {
				utils.ErrExit("glob %q: %s", inProgressPattern, err)
			}
			inCreatedPattern := fmt.Sprintf("%s/%s/data/*.C", exportDir, metaInfoDir)
			m2, err := filepath.Glob(inCreatedPattern)
			if err != nil {
				utils.ErrExit("glob %q: %s", inCreatedPattern, err)
			}
			// in progress are interrupted ones
			if len(m1) > 0 || len(m2) > 0 {
				time.Sleep(2 * time.Second)
			} else {
				log.Infof("No in-progress or newly-created splits. Import Done.")
				Done.Set()
			}
		} else {
			time.Sleep(5 * time.Second)
		}
	}

}

func roundRobinTargets(targets []*utils.Target, channel chan *utils.Target) {
	index := 0
	for Done.IsNotSet() {
		channel <- targets[index%len(targets)]
		index++
	}
}

//TODO: implement
func acquireImportLock() {
}

func generateSmallerSplits(taskQueue chan *fwk.SplitFileImportTask) {
	doneTables, interruptedTables, remainingTables, _ := getTablesToImport()

	log.Infof("doneTables: %s", doneTables)
	log.Infof("interruptedTables: %s", interruptedTables)
	log.Infof("remainingTables: %s", remainingTables)

	if target.TableList == "" { //no table-list is given by user
		importTables = append(interruptedTables, remainingTables...)
		allTables = append(importTables, doneTables...)
	} else {
		allTables = utils.CsvStringToSlice(target.TableList)

		//filter allTables to remove tables in case not present in --table-list flag
		for _, table := range allTables {
			//TODO: 'table' can be schema_name.table_name, so split and proceed
			notDone := true
			for _, t := range doneTables {
				if t == table {
					notDone = false
					break
				}
			}

			if notDone {
				importTables = append(importTables, table)
			}
		}
		if target.VerboseMode {
			fmt.Printf("given table-list: %v\n", target.TableList)
		}
	}

	sort.Strings(allTables)
	sort.Strings(importTables)

	log.Infof("allTables: %s", allTables)
	log.Infof("importTables: %s", importTables)

	if startClean { //start data migraiton from beginning
		fmt.Printf("Truncating all tables: %v\n", allTables)
		truncateTables(allTables)
		log.Infof("cleaning the database and %s/metadata/data directory", exportDir)
		utils.CleanDir(exportDir + "/metainfo/data")

		importTables = allTables //since all tables needs to imported now
	} else {
		//truncate tables with no primary key
		utils.PrintIfTrue("looking for tables without a Primary Key...\n", target.VerboseMode)
		for _, tableName := range importTables {
			if !checkPrimaryKey(tableName) {
				fmt.Printf("truncate table '%s' with NO Primary Key for import of data to restart from beginning...\n", tableName)
				utils.ClearMatchingFiles(exportDir + "/metainfo/data/" + tableName + ".[0-9]*.[0-9]*.[0-9]*.*") //correct and complete pattern to avoid matching cases with other table names
				truncateTables([]string{tableName})
			}
		}

	}

	if target.VerboseMode {
		fmt.Printf("all the tables to be imported: %v\n", allTables)
	}

	if !startClean {
		fmt.Printf("skipping already imported tables: %s\n", doneTables)
	}

	if target.VerboseMode {
		fmt.Printf("tables left to import: %v\n", importTables)
	}

	if len(importTables) == 0 {
		fmt.Printf("All the tables are already imported, nothing left to import\n")
		Done.Set()
		return
	} else {
		fmt.Printf("Preparing to import the tables: %v\n", importTables)
	}

	//Preparing the tablesProgressMetadata array
	initializeImportDataStatus(exportDir, importTables)

	go splitDataFiles(importTables, taskQueue)
}

func checkPrimaryKey(tableName string) bool {
	url := getTargetConnectionUri(&target)
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		utils.ErrExit("Unable to connect to database (uri=%s): %s", url, err)
	}
	defer conn.Close(context.Background())

	var table, schema string
	if len(strings.Split(tableName, ".")) == 2 {
		schema = strings.Split(tableName, ".")[0]
		table = strings.Split(tableName, ".")[1]
	} else {
		schema = target.Schema
		table = strings.Split(tableName, ".")[0]
	}

	checkPKSql := fmt.Sprintf(`SELECT * FROM information_schema.table_constraints
	WHERE constraint_type = 'PRIMARY KEY' AND table_name = '%s' AND table_schema = '%s';`, table, schema)
	// fmt.Println(checkPKSql)

	log.Infof("Running query on target DB: %s", checkPKSql)
	rows, err := conn.Query(context.Background(), checkPKSql)
	if err != nil {
		utils.ErrExit("error in querying to check PK on table %q: %s", table, err)
	}
	defer rows.Close()

	if rows.Next() {
		log.Infof("table %q has a PK", table)
		return true
	} else {
		log.Infof("table %q does not have a PK", table)
		return false
	}
}

func truncateTables(tables []string) {
	log.Infof("Truncating tables: %v", tables)
	connectionURI := target.GetConnectionUri()
	conn, err := pgx.Connect(context.Background(), connectionURI)
	if err != nil {
		utils.ErrExit("Unable to connect to database %q: %s", connectionURI, err)
	}
	defer conn.Close(context.Background())

	metaInfo := ExtractMetaInfo(exportDir)
	log.Infof("Source DB type: %q", metaInfo.SourceDBType)

	if metaInfo.SourceDBType != POSTGRESQL && target.Schema != YUGABYTEDB_DEFAULT_SCHEMA {
		setSchemaQuery := fmt.Sprintf("SET SCHEMA '%s'", target.Schema)
		_, err := conn.Exec(context.Background(), setSchemaQuery)
		if err != nil {
			utils.ErrExit("Failed to execute %q on target: %s", setSchemaQuery, err)
		}
	}

	for _, table := range tables {
		log.Infof("Truncating table: %q", table)
		if target.VerboseMode {
			fmt.Printf("Truncating table %s...\n", table)
		}
		truncateStmt := fmt.Sprintf("TRUNCATE TABLE %s CASCADE", table)
		_, err := conn.Exec(context.Background(), truncateStmt)
		if err != nil {
			utils.ErrExit("error while truncating table %q: %s", table, err)
		}
	}
}

func splitDataFiles(importTables []string, taskQueue chan *fwk.SplitFileImportTask) {
	log.Infof("Started goroutine: splitDataFiles")
	for _, t := range importTables {
		var tableNameUsed string //regenerating the table_data.sql filename, from extracted tableName
		parts := strings.Split(t, ".")
		sourceDBType := ExtractMetaInfo(exportDir).SourceDBType
		switch sourceDBType {
		case "postgresql":
			if len(parts) > 1 && parts[0] != "public" {
				tableNameUsed = strings.ToLower(parts[0]) + "."
			}
			tableNameUsed += strings.ToLower(parts[len(parts)-1])
		case "mysql":
			tableNameUsed = parts[len(parts)-1]
		case "oracle":
			tableNameUsed = strings.ToUpper(parts[len(parts)-1])
		}
		origDataFile := exportDir + "/data/" + tableNameUsed + "_data.sql"
		extractCopyStmtForTable(t, sourceDBType, origDataFile)
		log.Infof("Start splitting table %q: data-file: %q", t, origDataFile)

		log.Infof("Collect interrupted splits.")
		largestCreatedSplitSoFar := int64(0)
		largestOffsetSoFar := int64(0)
		fileFullySplit := false
		pattern := fmt.Sprintf("%s/%s/data/%s.[0-9]*.[0-9]*.[0-9]*.[CPD]", exportDir, metaInfoDir, t)
		matches, _ := filepath.Glob(pattern)
		// in progress are interrupted ones
		interruptedRegexStr := fmt.Sprintf(".+/%s\\.(\\d+)\\.(\\d+)\\.(\\d+)\\.[P]$", t)
		interruptedRegexp := regexp.MustCompile(interruptedRegexStr)
		for _, filepath := range matches {
			submatches := interruptedRegexp.FindAllStringSubmatch(filepath, -1)
			for _, match := range submatches {
				// This means a match. Submit the task with interrupted = true
				// fmt.Printf("filepath: %s, %v\n", filepath, match)
				/*
					offsets are 0-based, while numLines are 1-based
					offsetStart is the line in original datafile from where current split starts
					offsetEnd   is the line in original datafile from where next split starts
				*/
				splitNum, _ := strconv.ParseInt(match[1], 10, 64)
				offsetEnd, _ := strconv.ParseInt(match[2], 10, 64)
				numLines, _ := strconv.ParseInt(match[3], 10, 64)
				offsetStart := offsetEnd - numLines
				if splitNum == LAST_SPLIT_NUM {
					fileFullySplit = true
				}
				if splitNum > largestCreatedSplitSoFar {
					largestCreatedSplitSoFar = splitNum
				}
				if offsetEnd > largestOffsetSoFar {
					largestOffsetSoFar = offsetEnd
				}
				addASplitTask("", t, filepath, splitNum, offsetStart, offsetEnd, true, taskQueue)
			}
		}
		log.Infof("Collect files which were generated but processing did not start.")
		// schedule import task for them
		createdButNotStartedRegexStr := fmt.Sprintf(".+/%s\\.(\\d+)\\.(\\d+)\\.(\\d+)\\.[C]$", t)
		createdButNotStartedRegex := regexp.MustCompile(createdButNotStartedRegexStr)
		// fmt.Printf("created but not started regex = %s\n", createdButNotStartedRegex.String())
		for _, filepath := range matches {
			submatches := createdButNotStartedRegex.FindAllStringSubmatch(filepath, -1)
			for _, match := range submatches {
				// This means a match. Submit the task with interrupted = true
				splitNum, _ := strconv.ParseInt(match[1], 10, 64)
				offsetEnd, _ := strconv.ParseInt(match[2], 10, 64)
				numLines, _ := strconv.ParseInt(match[3], 10, 64)
				offsetStart := offsetEnd - numLines
				if splitNum == LAST_SPLIT_NUM {
					fileFullySplit = true
				}
				if splitNum > largestCreatedSplitSoFar {
					largestCreatedSplitSoFar = splitNum
				}
				if offsetEnd > largestOffsetSoFar {
					largestOffsetSoFar = offsetEnd
				}
				addASplitTask("", t, filepath, splitNum, offsetStart, offsetEnd, true, taskQueue)
			}
		}

		if !fileFullySplit {
			splitFilesForTable(origDataFile, t, taskQueue, largestCreatedSplitSoFar, largestOffsetSoFar)
		}
	}
	log.Info("All table data files are split.")
	GenerateSplitsDone.Set()
}

func splitFilesForTable(dataFile string, t string, taskQueue chan *fwk.SplitFileImportTask, largestSplit int64, largestOffset int64) {
	log.Infof("Split data file %q: tableName=%q, largestSplit=%v, largestOffset=%v", dataFile, t, largestSplit, largestOffset)
	splitNum := largestSplit + 1
	currTmpFileName := fmt.Sprintf("%s/%s/data/%s.%d.tmp", exportDir, metaInfoDir, t, splitNum)
	numLinesTaken := largestOffset
	numLinesInThisSplit := int64(0)
	forig, err := os.Open(dataFile)
	if err != nil {
		utils.ErrExit("open file %q: %s", dataFile, err)
	}
	defer forig.Close()

	r := bufio.NewReader(forig)
	sz := 0
	log.Infof("current temp file: %s", currTmpFileName)
	outfile, err := os.Create(currTmpFileName)
	if err != nil {
		utils.ErrExit("create file %q: %s", currTmpFileName, err)
	}

	log.Infof("Skipping %d lines from %q", largestOffset, dataFile)
	for i := int64(0); i < largestOffset; {
		line, err := utils.Readline(r)
		if err != nil { // EOF error is not possible here, since LAST_SPLIT is not created yet
			utils.ErrExit("read a line from %q: %s", dataFile, err)
		}
		if isDataLine(line) {
			i++
		}
	}

	// Create a buffered writer from the file
	bufferedWriter := bufio.NewWriter(outfile)
	var readLineErr error = nil
	var line string
	linesWrittenToBuffer := false
	for readLineErr == nil {
		line, readLineErr = utils.Readline(r)
		if readLineErr == nil && !isDataLine(line) {
			continue
		} else if readLineErr == nil { //increment the count only if line is valid
			numLinesTaken += 1
			numLinesInThisSplit += 1
		}

		if linesWrittenToBuffer {
			line = fmt.Sprintf("\n%s", line)
		}
		length, err := bufferedWriter.WriteString(line)
		linesWrittenToBuffer = true
		if err != nil {
			utils.ErrExit("Write line to %q: %s", outfile.Name(), err)
		}
		sz += length
		if sz >= FOUR_MB {
			err = bufferedWriter.Flush()
			if err != nil {
				utils.ErrExit("flush data in file %q: %s", outfile.Name(), err)
			}
			bufferedWriter.Reset(outfile)
			sz = 0
		}

		if numLinesInThisSplit == numLinesInASplit || readLineErr != nil {
			err = bufferedWriter.Flush()
			if err != nil {
				utils.ErrExit("flush data in file %q: %s", outfile.Name(), err)
			}
			outfile.Close()
			fileSplitNumber := splitNum
			if readLineErr == io.EOF {
				fileSplitNumber = LAST_SPLIT_NUM
				log.Infof("Preparing last split of %q", dataFile)
			} else if readLineErr != nil {
				utils.ErrExit("read line from data file %q: %s", dataFile, readLineErr)
			}

			offsetStart := numLinesTaken - numLinesInThisSplit
			offsetEnd := numLinesTaken
			splitFile := fmt.Sprintf("%s/%s/data/%s.%d.%d.%d.C",
				exportDir, metaInfoDir, t, fileSplitNumber, offsetEnd, numLinesInThisSplit)
			log.Infof("Renaming %q to %q", currTmpFileName, splitFile)
			err = os.Rename(currTmpFileName, splitFile)
			if err != nil {
				utils.ErrExit("rename %q to %q: %s", currTmpFileName, splitFile, err)
			}
			addASplitTask("", t, splitFile, splitNum, offsetStart, offsetEnd, false, taskQueue)

			if fileSplitNumber != 0 {
				splitNum += 1
				numLinesInThisSplit = 0
				linesWrittenToBuffer = false
				currTmpFileName = fmt.Sprintf("%s/%s/data/%s.%d.tmp", exportDir, metaInfoDir, t, splitNum)
				log.Infof("create next temp file: %q", currTmpFileName)
				outfile, err = os.Create(currTmpFileName)
				if err != nil {
					utils.ErrExit("create %q: %s", currTmpFileName, err)
				}
				bufferedWriter = bufio.NewWriter(outfile)
			}
		}
	}
	log.Infof("splitFilesForTable: done splitting data file %q for table %q", dataFile, t)
}

// Example: "SET client_encoding TO 'UTF8';"
var reSetTo = regexp.MustCompile(`(?i)SET \w+ TO .*;`)

// Example: "SET search_path = sakila_test,public;"
var reSetEq = regexp.MustCompile(`(?i)SET \w+ = .*;`)

// Example: `TRUNCATE TABLE "Foo";`
var reTruncate = regexp.MustCompile(`(?i)TRUNCATE TABLE ["'\w]*;`)

// Example: `COPY "Foo" ("v") FROM STDIN;`
var reCopy = regexp.MustCompile(`(?i)COPY .* FROM STDIN;`)

func isDataLine(line string) bool {
	return !(len(line) == 0 ||
		line == "\n" ||
		line == "\\." || line == "\\.\n" ||
		reSetTo.MatchString(line) ||
		reSetEq.MatchString(line) ||
		reTruncate.MatchString(line) ||
		reCopy.MatchString(line))
}

func addASplitTask(schemaName string, tableName string, filepath string, splitNumber int64, offsetStart int64, offsetEnd int64, interrupted bool,
	taskQueue chan *fwk.SplitFileImportTask) {
	var t fwk.SplitFileImportTask
	t.SchemaName = schemaName
	t.TableName = tableName
	t.SplitFilePath = filepath
	t.SplitNumber = splitNumber
	t.OffsetStart = offsetStart
	t.OffsetEnd = offsetEnd
	t.Interrupted = interrupted
	taskQueue <- &t
	log.Infof("Queued an import task: %s", spew.Sdump(t))
}

func executePostImportDataSqls() {
	/*
		Enable Sequences, if required
		Add Indexes, if required
	*/
	sequenceFilePath := exportDir + "/data/postdata.sql"
	indexesFilePath := exportDir + "/schema/tables/INDEXES_table.sql"

	if utils.FileOrFolderExists(sequenceFilePath) {
		fmt.Printf("setting resume value for sequences %10s", "")
		go utils.Wait("done\n", "")
		executeSqlFile(sequenceFilePath)
	}

	if utils.FileOrFolderExists(indexesFilePath) && target.ImportIndexesAfterData {
		fmt.Printf("creating indexes %10s", "")
		go utils.Wait("done\n", "")
		executeSqlFile(indexesFilePath)
	}

}

func getTablesToImport() ([]string, []string, []string, error) {
	metaInfoDir := fmt.Sprintf("%s/%s", exportDir, metaInfoDir)

	_, err := os.Stat(metaInfoDir)
	if err != nil {
		utils.ErrExit("metainfo dir is missing. Exiting.")
	}
	metaInfoDataDir := fmt.Sprintf("%s/data", metaInfoDir)
	_, err = os.Stat(metaInfoDataDir)
	if err != nil {
		utils.ErrExit("metainfo data dir is missing. Exiting.")
	}

	exportDataDonePath := metaInfoDir + "/flags/exportDataDone"
	_, err = os.Stat(exportDataDonePath)
	if err != nil {
		utils.ErrExit("Export is not done yet. Exiting.")
	}

	exportDataDir := fmt.Sprintf("%s/data", exportDir)
	_, err = os.Stat(exportDataDir)
	if err != nil {
		utils.ErrExit("Export data dir %s is missing. Exiting.\n", exportDataDir)
	}
	// Collect all the data files
	dataFilePatern := fmt.Sprintf("%s/*_data.sql", exportDataDir)
	datafiles, err := filepath.Glob(dataFilePatern)
	if err != nil {
		utils.ErrExit("find data files in %q: %s", exportDataDir, err)
	}

	pat := regexp.MustCompile(`.+/(\S+)_data.sql`)
	var tables []string
	for _, v := range datafiles {
		tablenameMatches := pat.FindAllStringSubmatch(v, -1)
		for _, match := range tablenameMatches {
			tables = append(tables, match[1]) //ora2pg data files named like TABLE_data.sql
		}
	}

	var doneTables []string
	var interruptedTables []string
	var remainingTables []string
	for _, t := range tables {

		donePattern := fmt.Sprintf("%s/%s.[0-9]*.[0-9]*.[0-9]*.D", metaInfoDataDir, t)
		interruptedPattern := fmt.Sprintf("%s/%s.[0-9]*.[0-9]*.[0-9]*.P", metaInfoDataDir, t)
		createdPattern := fmt.Sprintf("%s/%s.[0-9]*.[0-9]*.[0-9]*.C", metaInfoDataDir, t)

		doneMatches, _ := filepath.Glob(donePattern)
		interruptedMatches, _ := filepath.Glob(interruptedPattern)
		createdMatches, _ := filepath.Glob(createdPattern)

		//[Important] This function's return result is based on assumption that the rate of ingestion is slower than splitting
		if len(createdMatches) == 0 && len(interruptedMatches) == 0 && len(doneMatches) > 0 {
			doneTables = append(doneTables, t)
		} else if (len(createdMatches) > 0 && len(interruptedMatches)+len(doneMatches) == 0) ||
			(len(createdMatches)+len(interruptedMatches)+len(doneMatches) == 0) {
			remainingTables = append(remainingTables, t)
		} else {
			interruptedTables = append(interruptedTables, t)
		}
	}

	return doneTables, interruptedTables, remainingTables, nil
}

func doImport(taskQueue chan *fwk.SplitFileImportTask, parallelism int, targetChan chan *utils.Target) {
	if Done.IsSet() { //if import is already done, return
		log.Infof("Done is already set.")
		return
	}

	parallelImportCount := int64(0)

	importProgressContainer = ProgressContainer{
		container: mpb.New(),
	}
	go importDataStatus()

	for Done.IsNotSet() {
		select {
		case t := <-taskQueue:
			// fmt.Printf("Got taskfile = %s putting on parallel channel\n", t.SplitFilePath)
			// parallelImportChannel <- t
			for parallelImportCount >= int64(parallelism) {
				time.Sleep(time.Second * 2)
			}
			atomic.AddInt64(&parallelImportCount, 1)
			go doImportInParallel(t, targetChan, &parallelImportCount)
		default:
			// fmt.Printf("No file sleeping for 2 seconds\n")
			time.Sleep(2 * time.Second)
		}
	}

	importProgressContainer.container.Wait()
}

func doImportInParallel(t *fwk.SplitFileImportTask, targetChan chan *utils.Target, parallelImportCount *int64) {
	doOneImport(t, targetChan)
	atomic.AddInt64(parallelImportCount, -1)
}

func doOneImport(t *fwk.SplitFileImportTask, targetChan chan *utils.Target) {
	splitImportDone := false
	for !splitImportDone {
		select {
		case targetServer := <-targetChan:
			log.Infof("Importing %q using target node %q", t.SplitFilePath, targetServer.Host)
			//this is done to signal start progress bar for this table
			if tablesProgressMetadata[t.TableName].CountLiveRows == -1 {
				tablesProgressMetadata[t.TableName].CountLiveRows = 0
			}
			// Rename the file to .P
			inProgressFilePath := getInProgressFilePath(t)
			log.Infof("Renaming file from %q to %q", t.SplitFilePath, inProgressFilePath)
			err := os.Rename(t.SplitFilePath, inProgressFilePath)
			if err != nil {
				utils.ErrExit("rename %q to %q: %s", t.SplitFilePath, inProgressFilePath, err)
			}

			conn, err := pgx.Connect(context.Background(), targetServer.GetConnectionUri())
			if err != nil {
				utils.ErrExit("connect to YB node %q: %s", targetServer.Host, err)
			}
			defer conn.Close(context.Background())

			dbVersion := migration.SelectVersionQuery("yugabytedb", targetServer.GetConnectionUri())

			for i, statement := range IMPORT_SESSION_SETTERS {
				if checkSessionVariableSupported(i, dbVersion) {
					_, err := conn.Exec(context.Background(), statement)
					if err != nil {
						utils.ErrExit("import file %q: run query %q on %q: %s", inProgressFilePath, statement, targetServer.Host, err)
					}
				}
			}

			reader, err := os.Open(inProgressFilePath)
			if err != nil {
				utils.ErrExit("open %q: %s", inProgressFilePath, err)
			}

			//setting the schema so that COPY command can acesss the table
			if ExtractMetaInfo(exportDir).SourceDBType != POSTGRESQL && target.Schema != YUGABYTEDB_DEFAULT_SCHEMA {
				setSchemaQuery := fmt.Sprintf("SET SCHEMA '%s'", target.Schema)
				_, err := conn.Exec(context.Background(), setSchemaQuery)
				if err != nil {
					utils.ErrExit("run query %q on %q: %s", setSchemaQuery, targetServer.Host, err)
				}
			}

			copyCommand := getCopyCommand(t.TableName)

			res, err := conn.PgConn().CopyFrom(context.Background(), reader, copyCommand)
			rowsCount := res.RowsAffected()
			log.Infof("%q => %d rows affected", copyCommand, rowsCount)
			if err != nil {
				log.Warnf("COPY FROM file %q: %s", inProgressFilePath, err)
				if !strings.Contains(err.Error(), "violates unique constraint") {
					utils.ErrExit("COPY %q FROM file %q: %s", t.TableName, inProgressFilePath, err)
				} else { //in case of unique key violation error take row count from the split task
					rowsCount = t.OffsetEnd - t.OffsetStart
					log.Infof("assuming affected rows count %v", rowsCount)
				}
			}

			// update the import data status as soon as rows are copied
			incrementImportedRowCount(t.TableName, rowsCount)

			doneFilePath := getDoneFilePath(t)
			log.Infof("Renaming %q => %q", inProgressFilePath, doneFilePath)
			err = os.Rename(inProgressFilePath, doneFilePath)
			if err != nil {
				utils.ErrExit("rename %q => %q: %s", inProgressFilePath, doneFilePath, err)
			}

			err = os.Truncate(doneFilePath, 0)
			if err != nil {
				log.Warnf("truncate file %q: %s", doneFilePath, err)
			}
			splitImportDone = true
		default:
			// fmt.Printf("No server sleeping for 2 seconds\n")
			time.Sleep(200 * time.Millisecond)
		}
	}
}

/*
	function to check for session variable supported or not based on YBDB version
*/
func checkSessionVariableSupported(idx int, dbVersion string) bool {
	// YB version includes compatible postgres version also, for example: 11.2-YB-2.13.0.0-b0
	splits := strings.Split(dbVersion, "YB-")
	dbVersion = splits[len(splits)-1]

	if idx == 1 { //yb_disable_transactional_writes
		//only supported for these versions
		return strings.Compare(dbVersion, "2.8.1") == 0 || strings.Compare(dbVersion, "2.11.2") >= 0
	}
	return true
}

func executeSqlFile(file string) {
	log.Infof("Execute SQL file %q on target %q", file, target.Host)
	connectionURI := target.GetConnectionUri()
	conn, err := pgx.Connect(context.Background(), connectionURI)
	if err != nil {
		utils.WaitChannel <- 1
		<-utils.WaitChannel
		utils.ErrExit("connect to target db: %s", err)
	}
	defer conn.Close(context.Background())

	if ExtractMetaInfo(exportDir).SourceDBType != POSTGRESQL && target.Schema != YUGABYTEDB_DEFAULT_SCHEMA {
		setSchemaQuery := fmt.Sprintf("SET SCHEMA '%s'", target.Schema)
		_, err := conn.Exec(context.Background(), setSchemaQuery)
		if err != nil {
			utils.ErrExit("run query %q on target %q: %s", setSchemaQuery, target.Host, err)
		}
	}

	var errOccured = 0
	sqlStrArray := createSqlStrArray(file, "")
	for _, sqlStr := range sqlStrArray {
		log.Infof("Run query %q on target %q", sqlStr[1], target.Host)
		_, err := conn.Exec(context.Background(), sqlStr[0])
		if err != nil {
			log.Errorf("Run query %q on target %q: %s", sqlStr[1], target.Host, err)
			if strings.Contains(err.Error(), "already exists") {
				if !target.IgnoreIfExists {
					fmt.Printf("\b \n    %s\n", err.Error())
					fmt.Printf("    STATEMENT: %s\n", sqlStr[1])
					if !target.ContinueOnError {
						os.Exit(1)
					}
				}
			} else {
				errOccured = 1
				fmt.Printf("\b \n    %s\n", err.Error())
				fmt.Printf("    STATEMENT: %s\n", sqlStr[1])
				if !target.ContinueOnError { //default case
					fmt.Println(err)
					os.Exit(1)
				}
			}
		}
	}

	utils.WaitChannel <- errOccured
	<-utils.WaitChannel
}

func getInProgressFilePath(task *fwk.SplitFileImportTask) string {
	path := task.SplitFilePath
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	parts := strings.Split(base, ".")

	if len(parts) > 5 { //case when filename has schema also
		return fmt.Sprintf("%s/%s.%s.%s.%s.%s.P", dir, parts[0], parts[1], parts[2], parts[3], parts[4])
	} else {
		return fmt.Sprintf("%s/%s.%s.%s.%s.P", dir, parts[0], parts[1], parts[2], parts[3])
	}
}

func getDoneFilePath(task *fwk.SplitFileImportTask) string {
	path := task.SplitFilePath
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	parts := strings.Split(base, ".")

	if len(parts) > 5 { //case when filename has schema also
		return fmt.Sprintf("%s/%s.%s.%s.%s.%s.D", dir, parts[0], parts[1], parts[2], parts[3], parts[4])
	} else {
		return fmt.Sprintf("%s/%s.%s.%s.%s.D", dir, parts[0], parts[1], parts[2], parts[3])
	}
}

func incrementImportedRowCount(tableName string, rowsCopied int64) {
	tablesProgressMetadata[tableName].CountLiveRows += rowsCopied
	log.Infof("Table %q, total rows copied until now %v", tableName, tablesProgressMetadata[tableName].CountLiveRows)
}

func extractCopyStmtForTable(table string, sourceDBType string, fileToSearchIn string) {
	// pg_dump and ora2pg always have columns - "COPY table (col1, col2) FROM STDIN"
	copyCommandRegex := regexp.MustCompile(fmt.Sprintf(`(?i)COPY %s[\s]*(.*) FROM STDIN`, table))
	if sourceDBType == "postgresql" {
		// find the line from toc.txt file
		fileToSearchIn = exportDir + "/data/toc.txt"

		// if no schema then add public in tableName as it is there in postgres' toc file
		if len(strings.Split(table, ".")) == 1 {
			copyCommandRegex = regexp.MustCompile(fmt.Sprintf(`(?i)COPY public.%s[\s]*(.*) FROM STDIN`, table))
		}
	}

	file, err := os.Open(fileToSearchIn)
	if err != nil {
		utils.ErrExit("could not open file during extraction of copy stmt from file %q: %v", fileToSearchIn, err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, err := utils.Readline(reader)
		if err == io.EOF { // EOF will mean NO COPY command
			return
		} else if err != nil {
			utils.ErrExit("error while readline for extraction of copy stmt from file %q: %v", fileToSearchIn, err)
		}
		if copyCommandRegex.MatchString(line) {
			copyTableFromCommands[table] = line
			log.Infof("copyTableFromCommand for table %q is %q", table, line)
			return
		}
	}
}

func getCopyCommand(table string) string {
	if copyCommand, ok := copyTableFromCommands[table]; ok {
		return copyCommand
	} else {
		utils.ErrExit("No COPY command for table %q", table)
	}
	return "" // no-op
}

func init() {
	importCmd.AddCommand(importDataCmd)
	registerCommonImportFlags(importDataCmd)
}
