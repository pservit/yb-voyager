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
package srcdb

import (
	"strings"

	"github.com/samber/lo"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/utils"
)

type Source struct {
	DBType                   string        `json:"db_type"`
	Host                     string        `json:"host"`
	Port                     int           `json:"port"`
	User                     string        `json:"user"`
	Password                 string        `json:"password"`
	DBName                   string        `json:"db_name"`
	CDBName                  string        `json:"cdb_name"`
	DBSid                    string        `json:"db_sid"`
	CDBSid                   string        `json:"cdb_sid"`
	OracleHome               string        `json:"oracle_home"`
	TNSAlias                 string        `json:"tns_alias"`
	CDBTNSAlias              string        `json:"cdb_tns_alias"`
	Schema                   string        `json:"schema"`
	SSLMode                  string        `json:"ssl_mode"`
	SSLCertPath              string        `json:"ssl_cert_path"`
	SSLKey                   string        `json:"ssl_key"`
	SSLRootCert              string        `json:"ssl_root_cert"`
	SSLCRL                   string        `json:"ssl_crl"`
	SSLQueryString           string        `json:"ssl_query_string"`
	SSLKeyStore              string        `json:"ssl_keystore"`
	SSLKeyStorePassword      string        `json:"ssl_keystore_password"`
	SSLTrustStore            string        `json:"ssl_truststore"`
	SSLTrustStorePassword    string        `json:"ssl_truststore_password"`
	Uri                      string        `json:"uri"`
	NumConnections           int           `json:"num_connections"`
	VerboseMode              bool          `json:"verbose_mode"`
	TableList                string        `json:"table_list"`
	ExcludeTableList         string        `json:"exclude_table_list"`
	UseOrafce                utils.BoolStr `json:"use_orafce"`
	CommentsOnObjects        utils.BoolStr `json:"comments_on_objects"`
	DBVersion                string        `json:"db_version"`
	StrExportObjectTypesList string        `json:"str_export_object_types_list"`

	ExportObjectTypesList []string `json:"-"`
	sourceDB              SourceDB `json:"-"`
}

func (s *Source) Clone() *Source {
	newS := *s
	return &newS
}

func (s *Source) DB() SourceDB {
	if s.sourceDB == nil {
		s.sourceDB = newSourceDB(s)
	}
	return s.sourceDB
}

func (s *Source) GetOracleHome() string {
	if s.OracleHome != "" {
		return s.OracleHome
	} else {
		return "/usr/lib/oracle/21/client64"
	}
}

func (s *Source) IsOracleCDBSetup() bool {
	return (s.CDBName != "" || s.CDBTNSAlias != "" || s.CDBSid != "")
}

func (s *Source) ApplyExportSchemaObjectListFilter() {
	allowedObjects := utils.GetSchemaObjectList(s.DBType)
	if s.StrExportObjectTypesList == "" {
		s.ExportObjectTypesList = allowedObjects
		return
	}
	expectedObjectsSlice := strings.Split(s.StrExportObjectTypesList, ",")
	s.ExportObjectTypesList = lo.Filter(allowedObjects, func(objType string, _ int) bool { return utils.ContainsString(expectedObjectsSlice, objType) })
}

func parseSSLString(source *Source) {
	if source.SSLQueryString == "" {
		return
	}

	validParams := []string{"sslmode", "sslcert", "sslrootcert", "sslkey"}

	sslParams := strings.Split(source.SSLQueryString, "&")
	for _, param := range sslParams {
		slicedparam := strings.Split(param, "=")
		for i, checkParam := range validParams {
			if checkParam == slicedparam[0] {
				switch i {
				case 0:
					source.SSLMode = slicedparam[1]
				case 1:
					source.SSLCertPath = slicedparam[1]
				case 2:
					source.SSLRootCert = slicedparam[1]
				case 3:
					source.SSLKey = slicedparam[1]
				}
				break
			}
		}
	}
}
