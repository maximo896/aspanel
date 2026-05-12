package domaincache

import (
	"awvs-sqlmap-panel/models"
	"encoding/json"
	"net/url"
	"strings"

	"gorm.io/gorm"
)

func ApplySnapshot(db *gorm.DB, snapshot map[string]interface{}) (map[string]interface{}, error) {
	if db == nil || snapshot == nil {
		return snapshot, nil
	}
	domain, forceSSL, ok := snapshotScope(snapshot)
	if !ok {
		return snapshot, nil
	}
	if err := UpsertSnapshot(db, snapshot); err != nil {
		return snapshot, err
	}

	var cache models.DomainSQLMapCache
	if err := db.Where("domain = ? AND force_ssl = ?", domain, forceSSL).First(&cache).Error; err != nil {
		return snapshot, nil
	}

	merged := cloneMap(snapshot)
	cacheContent := decodeMap(cache.ContentJSON)
	cacheTree := decodeMap(cache.TreeJSON)
	merged["content"] = mergeContentMaps(cacheContent, normalizeContentMap(snapshot["content"]))
	merged["tree"] = mergeTreeMaps(cacheTree, buildTreeFromContent(normalizeContentMap(snapshot["content"])))
	return merged, nil
}

func LoadSnapshotByURL(db *gorm.DB, rawURL string) (map[string]interface{}, bool, error) {
	if db == nil {
		return nil, false, nil
	}
	domain, forceSSL, ok := scopeFromURL(rawURL)
	if !ok {
		return nil, false, nil
	}
	return LoadSnapshotByScope(db, domain, forceSSL)
}

func LoadSnapshotByScope(db *gorm.DB, domain string, forceSSL bool) (map[string]interface{}, bool, error) {
	if db == nil {
		return nil, false, nil
	}
	normalizedDomain := normalizeDomain(domain)
	if normalizedDomain == "" {
		return nil, false, nil
	}
	var cache models.DomainSQLMapCache
	if err := db.Where("domain = ? AND force_ssl = ?", normalizedDomain, forceSSL).First(&cache).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	content := decodeMap(cache.ContentJSON)
	tree := normalizeTreeMap(decodeMap(cache.TreeJSON))
	if len(content) == 0 && len(tree) == 0 {
		return nil, false, nil
	}
	return map[string]interface{}{
		"domain":    normalizedDomain,
		"force_ssl": forceSSL,
		"content":   mergeContentMaps(map[string]interface{}{}, content),
		"tree":      tree,
	}, true, nil
}

func UpsertSnapshot(db *gorm.DB, snapshot map[string]interface{}) error {
	if db == nil || snapshot == nil {
		return nil
	}
	domain, forceSSL, ok := snapshotScope(snapshot)
	if !ok {
		return nil
	}

	incomingContent := normalizeContentMap(snapshot["content"])
	incomingTree := mergeTreeMaps(buildTreeFromContent(incomingContent), normalizeTreeMap(snapshot["tree"]))
	if len(incomingContent) == 0 && len(incomingTree) == 0 {
		return nil
	}

	var cache models.DomainSQLMapCache
	err := db.Where("domain = ? AND force_ssl = ?", domain, forceSSL).First(&cache).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return err
	}

	cache.Domain = domain
	cache.ForceSSL = forceSSL
	cache.ContentJSON = encodeMap(mergeContentMaps(decodeMap(cache.ContentJSON), incomingContent))
	cache.TreeJSON = encodeMap(mergeTreeMaps(normalizeTreeMap(decodeMap(cache.TreeJSON)), incomingTree))

	if cache.ID == 0 {
		return db.Create(&cache).Error
	}
	return db.Save(&cache).Error
}

func snapshotScope(snapshot map[string]interface{}) (string, bool, bool) {
	domain := normalizeDomain(asString(snapshot["domain"]))
	if domain == "" {
		return "", false, false
	}
	return domain, asBool(snapshot["force_ssl"]), true
}

func scopeFromURL(rawURL string) (string, bool, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", false, false
	}
	host := normalizeDomain(parsed.Hostname())
	if host == "" {
		return "", false, false
	}
	return host, strings.EqualFold(parsed.Scheme, "https"), true
}

func normalizeDomain(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func asString(raw interface{}) string {
	if value, ok := raw.(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func asBool(raw interface{}) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes":
			return true
		}
	case float64:
		return value != 0
	case int:
		return value != 0
	}
	return false
}

func encodeMap(value map[string]interface{}) string {
	if len(value) == 0 {
		return "{}"
	}
	buf, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(buf)
}

func decodeMap(raw string) map[string]interface{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]interface{}{}
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return map[string]interface{}{}
	}
	return out
}

func cloneMap(raw map[string]interface{}) map[string]interface{} {
	if len(raw) == 0 {
		return map[string]interface{}{}
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		return map[string]interface{}{}
	}
	var out map[string]interface{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return map[string]interface{}{}
	}
	return out
}

func normalizeContentMap(raw interface{}) map[string]interface{} {
	content, _ := raw.(map[string]interface{})
	if len(content) == 0 {
		return map[string]interface{}{}
	}

	dbs := uniqueStrings(toStringSlice(content["dbs"]))
	currentDB := asString(content["current_db"])
	if currentDB != "" && !containsString(dbs, currentDB) {
		dbs = append([]string{currentDB}, dbs...)
	}

	return map[string]interface{}{
		"current_db": currentDB,
		"dbs":        stringsToInterfaces(dbs),
		"tables":     tablesToGeneric(normalizeTablesMap(content["tables"])),
		"columns":    columnsToGeneric(normalizeColumnsMap(content["columns"])),
	}
}

func mergeContentMaps(base, overlay map[string]interface{}) map[string]interface{} {
	baseContent := normalizeContentMap(base)
	overlayContent := normalizeContentMap(overlay)

	currentDB := asString(baseContent["current_db"])
	if candidate := asString(overlayContent["current_db"]); candidate != "" {
		currentDB = candidate
	}

	dbs := uniqueStrings(append(toStringSlice(baseContent["dbs"]), toStringSlice(overlayContent["dbs"])...))
	if currentDB != "" && !containsString(dbs, currentDB) {
		dbs = append([]string{currentDB}, dbs...)
	}

	return map[string]interface{}{
		"current_db": currentDB,
		"dbs":        stringsToInterfaces(dbs),
		"tables":     tablesToGeneric(mergeTablesMap(normalizeTablesMap(baseContent["tables"]), normalizeTablesMap(overlayContent["tables"]))),
		"columns":    columnsToGeneric(mergeColumnsMap(normalizeColumnsMap(baseContent["columns"]), normalizeColumnsMap(overlayContent["columns"]))),
	}
}

func toStringSlice(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		if stringsList, ok := raw.([]string); ok {
			return uniqueStrings(stringsList)
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		value := asString(item)
		if value != "" {
			out = append(out, value)
		}
	}
	return uniqueStrings(out)
}

func normalizeTablesMap(raw interface{}) map[string][]string {
	root, ok := raw.(map[string]interface{})
	if !ok {
		return map[string][]string{}
	}
	out := make(map[string][]string)
	for dbName, tableRaw := range root {
		dbKey := strings.TrimSpace(dbName)
		if dbKey == "" {
			continue
		}
		out[dbKey] = uniqueStrings(toStringSlice(tableRaw))
	}
	return out
}

func mergeTablesMap(base, overlay map[string][]string) map[string][]string {
	out := make(map[string][]string)
	for dbName, tables := range base {
		out[dbName] = uniqueStrings(tables)
	}
	for dbName, tables := range overlay {
		out[dbName] = uniqueStrings(append(out[dbName], tables...))
	}
	return out
}

func tablesToGeneric(raw map[string][]string) map[string]interface{} {
	out := make(map[string]interface{})
	for dbName, tables := range raw {
		out[dbName] = stringsToInterfaces(uniqueStrings(tables))
	}
	return out
}

func normalizeColumnsMap(raw interface{}) map[string]map[string]map[string]string {
	root, ok := raw.(map[string]interface{})
	if !ok {
		return map[string]map[string]map[string]string{}
	}
	out := make(map[string]map[string]map[string]string)
	for dbName, tableRaw := range root {
		dbKey := strings.TrimSpace(dbName)
		if dbKey == "" {
			continue
		}
		tableMap, ok := tableRaw.(map[string]interface{})
		if !ok {
			continue
		}
		out[dbKey] = make(map[string]map[string]string)
		for tableName, columnRaw := range tableMap {
			tableKey := strings.TrimSpace(tableName)
			if tableKey == "" {
				continue
			}
			columnMap, ok := columnRaw.(map[string]interface{})
			if !ok {
				continue
			}
			out[dbKey][tableKey] = make(map[string]string)
			for columnName, columnType := range columnMap {
				columnKey := strings.TrimSpace(columnName)
				if columnKey == "" {
					continue
				}
				out[dbKey][tableKey][columnKey] = asString(columnType)
			}
		}
	}
	return out
}

func mergeColumnsMap(base, overlay map[string]map[string]map[string]string) map[string]map[string]map[string]string {
	out := make(map[string]map[string]map[string]string)
	for dbName, tableMap := range base {
		if _, ok := out[dbName]; !ok {
			out[dbName] = make(map[string]map[string]string)
		}
		for tableName, columnMap := range tableMap {
			if _, ok := out[dbName][tableName]; !ok {
				out[dbName][tableName] = make(map[string]string)
			}
			for columnName, columnType := range columnMap {
				out[dbName][tableName][columnName] = columnType
			}
		}
	}
	for dbName, tableMap := range overlay {
		if _, ok := out[dbName]; !ok {
			out[dbName] = make(map[string]map[string]string)
		}
		for tableName, columnMap := range tableMap {
			if _, ok := out[dbName][tableName]; !ok {
				out[dbName][tableName] = make(map[string]string)
			}
			for columnName, columnType := range columnMap {
				out[dbName][tableName][columnName] = columnType
			}
		}
	}
	return out
}

func columnsToGeneric(raw map[string]map[string]map[string]string) map[string]interface{} {
	out := make(map[string]interface{})
	for dbName, tableMap := range raw {
		tableOut := make(map[string]interface{})
		for tableName, columnMap := range tableMap {
			columnOut := make(map[string]interface{})
			for columnName, columnType := range columnMap {
				columnOut[columnName] = columnType
			}
			tableOut[tableName] = columnOut
		}
		out[dbName] = tableOut
	}
	return out
}

func normalizeTreeMap(raw interface{}) map[string]interface{} {
	tree, _ := raw.(map[string]interface{})
	if len(tree) == 0 {
		return map[string]interface{}{}
	}
	normalized := buildTreeFromContent(map[string]interface{}{})
	baseDatabases := normalizeTreeDatabases(normalized["databases"])
	incomingDatabases := normalizeTreeDatabases(tree["databases"])
	return map[string]interface{}{
		"databases": treeDatabasesToGeneric(mergeTreeDatabases(baseDatabases, incomingDatabases)),
	}
}

func buildTreeFromContent(content map[string]interface{}) map[string]interface{} {
	dbMap := make(map[string]*treeDatabase)
	currentDB := asString(content["current_db"])
	for _, dbName := range toStringSlice(content["dbs"]) {
		dbMap[dbName] = ensureTreeDatabase(dbMap, dbName)
	}
	if currentDB != "" {
		dbMap[currentDB] = ensureTreeDatabase(dbMap, currentDB)
	}

	for dbName, tables := range normalizeTablesMap(content["tables"]) {
		dbItem := ensureTreeDatabase(dbMap, dbName)
		for _, tableName := range tables {
			ensureTreeTable(dbItem, tableName)
		}
	}

	for dbName, tableMap := range normalizeColumnsMap(content["columns"]) {
		dbItem := ensureTreeDatabase(dbMap, dbName)
		for tableName, columnMap := range tableMap {
			tableItem := ensureTreeTable(dbItem, tableName)
			for columnName, columnType := range columnMap {
				if !containsString(tableItem.Columns, columnName) {
					tableItem.Columns = append(tableItem.Columns, columnName)
				}
				if tableItem.ColumnTypes == nil {
					tableItem.ColumnTypes = make(map[string]string)
				}
				tableItem.ColumnTypes[columnName] = columnType
			}
		}
	}

	return map[string]interface{}{
		"databases": treeDatabasesToGeneric(dbMap),
	}
}

type treeDatabase struct {
	Name          string
	PriorityTable string
	Tables        map[string]*treeTable
}

type treeTable struct {
	Name        string
	Columns     []string
	ColumnTypes map[string]string
	Rows        []map[string]interface{}
	Priority    bool
}

func ensureTreeDatabase(dbMap map[string]*treeDatabase, dbName string) *treeDatabase {
	key := strings.TrimSpace(dbName)
	if key == "" {
		key = "current"
	}
	if _, ok := dbMap[key]; !ok {
		dbMap[key] = &treeDatabase{
			Name:   key,
			Tables: make(map[string]*treeTable),
		}
	}
	return dbMap[key]
}

func ensureTreeTable(database *treeDatabase, tableName string) *treeTable {
	key := strings.TrimSpace(tableName)
	if key == "" {
		key = "table"
	}
	if database.Tables == nil {
		database.Tables = make(map[string]*treeTable)
	}
	if _, ok := database.Tables[key]; !ok {
		database.Tables[key] = &treeTable{
			Name:        key,
			Columns:     []string{},
			ColumnTypes: make(map[string]string),
			Rows:        []map[string]interface{}{},
		}
	}
	return database.Tables[key]
}

func normalizeTreeDatabases(raw interface{}) map[string]*treeDatabase {
	items, _ := raw.([]interface{})
	out := make(map[string]*treeDatabase)
	for _, item := range items {
		database, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		dbItem := ensureTreeDatabase(out, asString(database["name"]))
		if priorityTable := asString(database["priority_table"]); priorityTable != "" {
			dbItem.PriorityTable = priorityTable
		}
		tableItems, _ := database["tables"].([]interface{})
		for _, tableRaw := range tableItems {
			table, ok := tableRaw.(map[string]interface{})
			if !ok {
				continue
			}
			tableItem := ensureTreeTable(dbItem, asString(table["name"]))
			tableItem.Columns = uniqueStrings(append(tableItem.Columns, toStringSlice(table["columns"])...))
			if columnTypes, ok := table["column_types"].(map[string]interface{}); ok {
				for columnName, columnType := range columnTypes {
					tableItem.ColumnTypes[columnName] = asString(columnType)
				}
			}
			rowItems, _ := table["rows"].([]interface{})
			for _, rowRaw := range rowItems {
				rowMap, ok := rowRaw.(map[string]interface{})
				if !ok {
					continue
				}
				if !containsRow(tableItem.Rows, rowMap) {
					tableItem.Rows = append(tableItem.Rows, rowMap)
				}
			}
			tableItem.Priority = tableItem.Priority || asBool(table["priority"])
		}
	}
	return out
}

func mergeTreeMaps(base, overlay map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"databases": treeDatabasesToGeneric(mergeTreeDatabases(normalizeTreeDatabases(base["databases"]), normalizeTreeDatabases(overlay["databases"]))),
	}
}

func mergeTreeDatabases(base, overlay map[string]*treeDatabase) map[string]*treeDatabase {
	out := make(map[string]*treeDatabase)
	for dbName, database := range base {
		out[dbName] = cloneTreeDatabase(database)
	}
	for dbName, database := range overlay {
		target, ok := out[dbName]
		if !ok {
			out[dbName] = cloneTreeDatabase(database)
			continue
		}
		if database.PriorityTable != "" {
			target.PriorityTable = database.PriorityTable
		}
		for tableName, table := range database.Tables {
			targetTable, exists := target.Tables[tableName]
			if !exists {
				target.Tables[tableName] = cloneTreeTable(table)
				continue
			}
			targetTable.Columns = uniqueStrings(append(targetTable.Columns, table.Columns...))
			if targetTable.ColumnTypes == nil {
				targetTable.ColumnTypes = make(map[string]string)
			}
			for columnName, columnType := range table.ColumnTypes {
				targetTable.ColumnTypes[columnName] = columnType
			}
			for _, row := range table.Rows {
				if !containsRow(targetTable.Rows, row) {
					targetTable.Rows = append(targetTable.Rows, row)
				}
			}
			targetTable.Priority = targetTable.Priority || table.Priority
		}
	}
	return out
}

func cloneTreeDatabase(database *treeDatabase) *treeDatabase {
	if database == nil {
		return &treeDatabase{Tables: make(map[string]*treeTable)}
	}
	out := &treeDatabase{
		Name:          database.Name,
		PriorityTable: database.PriorityTable,
		Tables:        make(map[string]*treeTable),
	}
	for tableName, table := range database.Tables {
		out.Tables[tableName] = cloneTreeTable(table)
	}
	return out
}

func cloneTreeTable(table *treeTable) *treeTable {
	if table == nil {
		return &treeTable{ColumnTypes: make(map[string]string)}
	}
	out := &treeTable{
		Name:        table.Name,
		Columns:     append([]string{}, table.Columns...),
		ColumnTypes: make(map[string]string),
		Rows:        make([]map[string]interface{}, 0, len(table.Rows)),
		Priority:    table.Priority,
	}
	for columnName, columnType := range table.ColumnTypes {
		out.ColumnTypes[columnName] = columnType
	}
	for _, row := range table.Rows {
		out.Rows = append(out.Rows, cloneMap(row))
	}
	return out
}

func treeDatabasesToGeneric(dbMap map[string]*treeDatabase) []interface{} {
	names := make([]string, 0, len(dbMap))
	for dbName := range dbMap {
		names = append(names, dbName)
	}
	sortStrings(names)
	out := make([]interface{}, 0, len(names))
	for _, dbName := range names {
		database := dbMap[dbName]
		tableNames := make([]string, 0, len(database.Tables))
		for tableName := range database.Tables {
			tableNames = append(tableNames, tableName)
		}
		sortStrings(tableNames)
		tables := make([]interface{}, 0, len(tableNames))
		for _, tableName := range tableNames {
			table := database.Tables[tableName]
			sortStrings(table.Columns)
			rows := make([]interface{}, 0, len(table.Rows))
			for _, row := range table.Rows {
				rows = append(rows, cloneMap(row))
			}
			columnTypes := make(map[string]interface{})
			for columnName, columnType := range table.ColumnTypes {
				columnTypes[columnName] = columnType
			}
			tables = append(tables, map[string]interface{}{
				"name":         table.Name,
				"columns":      stringsToInterfaces(table.Columns),
				"column_types": columnTypes,
				"rows":         rows,
				"priority":     table.Priority || database.PriorityTable == table.Name,
			})
		}
		out = append(out, map[string]interface{}{
			"name":           database.Name,
			"priority_table": database.PriorityTable,
			"tables":         tables,
		})
	}
	return out
}

func containsRow(rows []map[string]interface{}, candidate map[string]interface{}) bool {
	candidateJSON := encodeMap(candidate)
	for _, row := range rows {
		if encodeMap(row) == candidateJSON {
			return true
		}
	}
	return false
}

func stringsToInterfaces(items []string) []interface{} {
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func uniqueStrings(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{})
	for _, item := range items {
		value := strings.TrimSpace(item)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsString(items []string, target string) bool {
	needle := strings.ToLower(strings.TrimSpace(target))
	for _, item := range items {
		if strings.ToLower(strings.TrimSpace(item)) == needle {
			return true
		}
	}
	return false
}

func sortStrings(items []string) {
	if len(items) < 2 {
		return
	}
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if strings.ToLower(items[j]) < strings.ToLower(items[i]) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}
