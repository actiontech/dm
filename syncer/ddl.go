// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"fmt"
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb-enterprise-tools/pkg/filter"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/parser"
)

// trimCtrlChars returns a slice of the string s with all leading
// and trailing control characters removed.
func trimCtrlChars(s string) string {
	f := func(r rune) bool {
		// All entries in the ASCII table below code 32 (technically the C0 control code set) are of this kind,
		// including CR and LF used to separate lines of text. The code 127 (DEL) is also a control character.
		// Reference: https://en.wikipedia.org/wiki/Control_character
		return r < 32 || r == 127
	}

	return strings.TrimFunc(s, f)
}

// resolveDDLSQL resolve to one ddl sql
// example: drop table test.a,test2.b -> drop table test.a; drop table test2.b;
func resolveDDLSQL(sql string) (sqls []string, err error) {
	sql = trimCtrlChars(sql)
	// We use Parse not ParseOneStmt here, because sometimes we got a commented out ddl which can't be parsed
	// by ParseOneStmt(it's a limitation of tidb parser.)
	stmts, err := parser.New().Parse(sql, "", "")
	if err != nil {
		return []string{sql}, errors.Errorf("error while parsing sql: %s, err:%s", sql, err)
	}

	if len(stmts) == 0 {
		return nil, nil
	}

	stmt := stmts[0]
	_, isDDL := stmt.(ast.DDLNode)
	if !isDDL {
		// let sqls be empty
		return sqls, nil
	}

	switch v := stmt.(type) {
	case *ast.DropTableStmt:
		var ex string
		if v.IfExists {
			ex = "IF EXISTS "
		}
		for _, t := range v.Tables {
			var db string
			if t.Schema.O != "" {
				db = fmt.Sprintf("`%s`.", t.Schema.O)
			}
			s := fmt.Sprintf("DROP TABLE %s%s`%s`", ex, db, t.Name.O)
			sqls = append(sqls, s)
		}
	case *ast.AlterTableStmt:
		tempSpecs := v.Specs
		newTable := &ast.TableName{}
		log.Warnf("will split alter table statement: %v", sql)
		for i := range tempSpecs {
			v.Specs = tempSpecs[i : i+1]
			splitted := alterTableStmtToSQL(v, newTable)
			log.Warnf("splitted alter table statement: %v", splitted)
			sqls = append(sqls, splitted...)
		}
	case *ast.RenameTableStmt:
		for _, t2t := range v.TableToTables {
			sqlNew := fmt.Sprintf("RENAME TABLE %s TO %s", tableNameToSQL(t2t.OldTable), tableNameToSQL(t2t.NewTable))
			sqls = append(sqls, sqlNew)
		}

	default:
		sqls = append(sqls, sql)
	}
	return sqls, nil
}

// todo: fix the ugly code, use ast to rename table
func genDDLSQL(sql string, originTableNames []*filter.Table, targetTableNames []*filter.Table) (string, error) {
	addUseDatabase := func(sql string, dbName string) string {
		return fmt.Sprintf("USE `%s`; %s;", dbName, sql)
	}

	stmt, err := parser.New().ParseOneStmt(sql, "", "")
	if err != nil {
		return "", errors.Trace(err)
	}

	if notNeedRoute(originTableNames, targetTableNames) {
		_, isCreateDatabase := stmt.(*ast.CreateDatabaseStmt)
		if isCreateDatabase {
			return fmt.Sprintf("%s;", sql), nil
		}

		return addUseDatabase(sql, originTableNames[0].Schema), nil
	}

	switch stmt.(type) {
	case *ast.CreateDatabaseStmt:
		sqlPrefix := createDatabaseRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		return createDatabaseRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`", sqlPrefix[:index], targetTableNames[0].Schema)), nil

	case *ast.DropDatabaseStmt:
		sqlPrefix := dropDatabaseRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		return dropDatabaseRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`", sqlPrefix[:index], targetTableNames[0].Schema)), nil

	case *ast.CreateTableStmt:
		var (
			sqlPrefix string
			index     int
		)
		// replace `like schema.table` section
		if len(originTableNames) == 2 {
			sqlPrefix = createTableLikeRegex.FindString(sql)
			index = findLastWord(sqlPrefix)
			endChars := ""
			if sqlPrefix[len(sqlPrefix)-1] == ')' {
				endChars = ")"
			}
			sql = createTableLikeRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`%s", sqlPrefix[:index], targetTableNames[1].Schema, targetTableNames[1].Name, endChars))
		}
		// replce `create table schame.table` section
		sqlPrefix = createTableRegex.FindString(sql)
		index = findLastWord(sqlPrefix)
		endChars := findTableDefineIndex(sqlPrefix[index:])
		sql = createTableRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`%s", sqlPrefix[:index], targetTableNames[0].Schema, targetTableNames[0].Name, endChars))

	case *ast.DropTableStmt:
		sqlPrefix := dropTableRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		sql = dropTableRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`", sqlPrefix[:index], targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.TruncateTableStmt:
		sql = fmt.Sprintf("TRUNCATE TABLE `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name)

	case *ast.AlterTableStmt:
		// RENAME [TO|AS] new_tbl_name
		if len(originTableNames) == 2 {
			index := findLastWord(sql)
			sql = fmt.Sprintf("%s`%s`.`%s`", sql[:index], targetTableNames[1].Schema, targetTableNames[1].Name)
		}
		sql = alterTableRegex.ReplaceAllString(sql, fmt.Sprintf("ALTER TABLE `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.RenameTableStmt:
		return fmt.Sprintf("RENAME TABLE `%s`.`%s` TO `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name,
			targetTableNames[1].Schema, targetTableNames[1].Name), nil

	case *ast.CreateIndexStmt:
		sql = createIndexDDLRegex.ReplaceAllString(sql, fmt.Sprintf("ON `%s`.`%s` (", targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.DropIndexStmt:
		sql = dropIndexDDLRegex.ReplaceAllString(sql, fmt.Sprintf("ON `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name))

	default:
		return "", errors.Errorf("unkown type ddl %s", sql)
	}

	return addUseDatabase(sql, targetTableNames[0].Schema), nil
}

func notNeedRoute(originTableNames []*filter.Table, targetTableNames []*filter.Table) bool {
	for index, originTableName := range originTableNames {
		targetTableName := targetTableNames[index]
		if originTableName.Schema != targetTableName.Schema {
			return false
		}
		if originTableName.Name != targetTableName.Name {
			return false
		}
	}
	return true
}

func findLastWord(literal string) int {
	index := len(literal) - 1
	for index >= 0 && literal[index] == ' ' {
		index--
	}

	for index >= 0 {
		if literal[index-1] == ' ' {
			return index
		}
		index--
	}
	return index
}

func findTableDefineIndex(literal string) string {
	for i := range literal {
		if literal[i] == '(' {
			return literal[i:]
		}
	}
	return ""
}

func genTableName(schema string, table string) *filter.Table {
	return &filter.Table{Schema: schema, Name: table}

}

// the result contains [tableName] excepted create table like and rename table
// for `create table like` DDL, result contains [sourceTableName, sourceRefTableName]
// for rename table ddl, result contains [targetOldTableName, sourceNewTableName]
func parserDDLTableNames(sql string) ([]*filter.Table, error) {
	stmt, err := parser.New().ParseOneStmt(sql, "", "")
	if err != nil {
		return nil, errors.Trace(err)
	}

	var res []*filter.Table
	switch v := stmt.(type) {
	case *ast.CreateDatabaseStmt:
		res = append(res, genTableName(v.Name, ""))
	case *ast.DropDatabaseStmt:
		res = append(res, genTableName(v.Name, ""))
	case *ast.CreateTableStmt:
		res = append(res, genTableName(v.Table.Schema.L, v.Table.Name.L))
		if v.ReferTable != nil {
			res = append(res, genTableName(v.ReferTable.Schema.L, v.ReferTable.Name.L))
		}
	case *ast.DropTableStmt:
		if len(v.Tables) != 1 {
			return res, errors.Errorf("drop table with multiple tables, may resovle ddl sql failed")
		}
		res = append(res, genTableName(v.Tables[0].Schema.L, v.Tables[0].Name.L))
	case *ast.TruncateTableStmt:
		res = append(res, genTableName(v.Table.Schema.L, v.Table.Name.L))
	case *ast.AlterTableStmt:
		res = append(res, genTableName(v.Table.Schema.L, v.Table.Name.L))
		if v.Specs[0].NewTable != nil {
			res = append(res, genTableName(v.Specs[0].NewTable.Schema.L, v.Specs[0].NewTable.Name.L))
		}
	case *ast.RenameTableStmt:
		res = append(res, genTableName(v.OldTable.Schema.L, v.OldTable.Name.L))
		res = append(res, genTableName(v.NewTable.Schema.L, v.NewTable.Name.L))
	case *ast.CreateIndexStmt:
		res = append(res, genTableName(v.Table.Schema.L, v.Table.Name.L))
	case *ast.DropIndexStmt:
		res = append(res, genTableName(v.Table.Schema.L, v.Table.Name.L))
	default:
		return res, errors.Errorf("unkown type ddl %s", sql)
	}

	return res, nil
}
