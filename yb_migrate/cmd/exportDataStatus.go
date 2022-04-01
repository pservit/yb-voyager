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
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/yugabyte/ybm/yb_migrate/src/utils"

	"github.com/fatih/color"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

func exportDataStatus(ctx context.Context, tablesMetadata []utils.TableProgressMetadata, quitChan chan bool) {
	quitChan2 := make(chan bool)
	quit := false
	go func() {
		quit = <-quitChan2
		if quit {
			quitChan <- true
		}
	}()

	numTables := len(tablesMetadata)
	progressContainer := mpb.NewWithContext(ctx)

	doneCount := 0
	var exportedTables []string

	for doneCount < numTables && !quit { //TODO: wait for export data to start
		for i := 0; i < numTables && !quit; i++ {
			if tablesMetadata[i].Status == utils.TABLE_MIGRATION_NOT_STARTED && (utils.FileOrFolderExists(tablesMetadata[i].InProgressFilePath) ||
				utils.FileOrFolderExists(tablesMetadata[i].FinalFilePath)) {
				tablesMetadata[i].Status = utils.TABLE_MIGRATION_IN_PROGRESS
				go startExportPB(progressContainer, &tablesMetadata[i], quitChan2)

			} else if tablesMetadata[i].Status == utils.TABLE_MIGRATION_DONE {
				tablesMetadata[i].Status = utils.TABLE_MIGRATION_COMPLETED
				exportedTables = append(exportedTables, tablesMetadata[i].FullTableName)
				doneCount++
				if doneCount == numTables {
					break
				}
			}

			//for failure/error handling. TODO: test it more
			if ctx.Err() != nil {
				fmt.Println(ctx.Err())
				break
			}
		}

		if ctx.Err() != nil {
			fmt.Println(ctx.Err())
			break
		}
		time.Sleep(1 * time.Second)
	}

	progressContainer.Wait() //shouldn't be needed as the previous loop is doing the same

	printExportedTables(exportedTables)

	//TODO: print remaining/unable-to-export tables
}

func startExportPB(progressContainer *mpb.Progress, tableMetadata *utils.TableProgressMetadata, quitChan chan bool) {
	// defer utils.WaitGroup.Done()

	var tableName string
	if source.DBType == POSTGRESQL && tableMetadata.TableSchema != "public" {
		tableName = tableMetadata.TableSchema + "." + tableMetadata.TableName
	} else {
		tableName = tableMetadata.TableName
	}

	total := int64(100)

	bar := progressContainer.AddBar(total,
		mpb.BarFillerClearOnComplete(),
		// mpb.BarRemoveOnComplete(),
		mpb.PrependDecorators(
			decor.Name(tableName),
		),
		mpb.AppendDecorators(
			// decor.Percentage(decor.WCSyncSpaceR),
			decor.OnComplete(
				decor.NewPercentage("%.2f", decor.WCSyncSpaceR), "completed",
			),
			decor.OnComplete(
				//TODO: default feature by package, need to verify the correctness/algorithm for ETA
				decor.AverageETA(decor.ET_STYLE_GO), "",
			),
		),
	)

	if tableMetadata.CountTotalRows == 0 {
		bar.IncrInt64(100)
		tableMetadata.Status = utils.TABLE_MIGRATION_DONE
		return
	}

	tableDataFileName := tableMetadata.InProgressFilePath
	if utils.FileOrFolderExists(tableMetadata.FinalFilePath) {
		tableDataFileName = tableMetadata.FinalFilePath
	}
	tableDataFile, err := os.Open(tableDataFileName)
	if err != nil {
		fmt.Println(err)
		quitChan <- true
		runtime.Goexit()
	}
	defer tableDataFile.Close()

	reader := bufio.NewReader(tableDataFile)
	// var prevLine string

	go func() { //for continuously increasing PB percentage
		for !bar.Completed() {
			PercentageValueFloat := float64(tableMetadata.CountLiveRows) / float64(tableMetadata.CountTotalRows) * 100
			PercentageValueInt64 := int64(PercentageValueFloat)
			incrementValue := (PercentageValueInt64) - bar.Current()
			bar.IncrInt64(incrementValue)
			time.Sleep(time.Millisecond * 500)
		}
	}()

	var line string
	var readLineErr error
	for !checkForEndOfFile(&source, tableMetadata, line) {
		for {
			line, readLineErr = reader.ReadString('\n')
			if readLineErr == io.EOF {
				break
			} else if readLineErr != nil { //error other than EOF
				panic(readLineErr)
			}

			if strings.HasPrefix(line, "\\.") { //break loop to execute checkForEndOfFile()
				break
			} else if isDataLine(line) {
				tableMetadata.CountLiveRows += 1
			}
		}
	}

	/*
		Below extra step to count rows because there may be still a possibility that some rows left uncounted before EOF
		1. if previous loop breaks because of fileName changes and before counting all rows.
		2. Based on - even after file rename the file access with reader stays and can count remaining lines in the file
		(Mainly for Oracle, MySQL)
	*/

	for {
		line, readLineErr := reader.ReadString('\n')
		if readLineErr == io.EOF {
			break
		} else if readLineErr != nil { //error other than EOF
			panic(readLineErr)
		}
		if isDataLine(line) {
			tableMetadata.CountLiveRows += 1
		}
	}
	if !bar.Completed() {
		bar.IncrBy(100) // Completing remaining progress bar to continue the execution.
	}
	tableMetadata.Status = utils.TABLE_MIGRATION_DONE
}

func checkForEndOfFile(source *utils.Source, tableMetadata *utils.TableProgressMetadata, line string) bool {
	if source.DBType == "postgresql" {
		if strings.HasPrefix(line, "\\.") {
			// fmt.Fprintf(debugFile, "checkForEOF done for table:%s line:%s, tablefile: %s\n", tableMetadata.TableName, line, tableMetadata.FinalFilePath)
			return true
		}
	} else if source.DBType == "oracle" || source.DBType == "mysql" {
		if !utils.FileOrFolderExists(tableMetadata.InProgressFilePath) && utils.FileOrFolderExists(tableMetadata.FinalFilePath) {
			// fmt.Fprintf(debugFile, "checkForEOF done for table:%s line:%s, tablefile: %s\n", tableMetadata.TableName, line, tableMetadata.FinalFilePath)
			return true
		}
	}
	return false
}

func printExportedTables(exportedTables []string) {
	output := "Exported tables:- {"
	nt := len(exportedTables)
	for i := 0; i < nt; i++ {
		output += exportedTables[i]
		if i < nt-1 {
			output += ",  "
		}

	}

	output += "}"

	color.Yellow(output)
}
