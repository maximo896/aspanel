package paneldb

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"awvs-sqlmap-panel/models"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	containerName = "awvs-sqlmap-mysql"
	mysqlImage    = "mariadb:11"
	mysqlPort     = "3307"
	mysqlDatabase = "awvs_sqlmap_panel"
	mysqlUser     = "panel"

	sqliteDBFile   = "panel.db"
	mysqlDataDir   = "mysql-data"
	mysqlEnvFile   = "data/mysql.env"
	migrationStamp = "data/sqlite-to-mysql.migrated"
)

type mysqlConfig struct {
	RootPassword string
	Password     string
}

func Open() (*gorm.DB, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	cfg, err := loadOrCreateMySQLConfig(cwd)
	if err != nil {
		return nil, err
	}

	sqlitePath := filepath.Join(cwd, sqliteDBFile)
	sqliteExists := fileExists(sqlitePath)
	containerExists, err := dockerContainerExists()
	if err != nil {
		return nil, err
	}

	switch {
	case sqliteExists && !containerExists:
		log.Printf("[db] found %s and no project MySQL container; starting MySQL and migrating", sqliteDBFile)
	case containerExists:
		log.Printf("[db] found project MySQL container; using MySQL")
	default:
		log.Printf("[db] no %s and no project MySQL container; starting fresh MySQL", sqliteDBFile)
	}

	if err := ensureMySQLContainer(cwd, cfg, containerExists); err != nil {
		return nil, err
	}

	db, err := openMySQLWithRetry(cfg, 90*time.Second)
	if err != nil {
		return nil, err
	}
	configurePool(db)

	if err := db.AutoMigrate(models.AllModels()...); err != nil {
		return nil, err
	}

	if sqliteExists {
		if err := migrateSQLiteIfNeeded(db, sqlitePath); err != nil {
			return nil, err
		}
	}

	return db, nil
}

func loadOrCreateMySQLConfig(baseDir string) (mysqlConfig, error) {
	path := filepath.Join(baseDir, mysqlEnvFile)
	cfg := mysqlConfig{}
	if fileExists(path) {
		values, err := readEnvFile(path)
		if err != nil {
			return cfg, err
		}
		cfg.RootPassword = values["MYSQL_ROOT_PASSWORD"]
		cfg.Password = values["MYSQL_PASSWORD"]
		if cfg.RootPassword != "" && cfg.Password != "" {
			return cfg, nil
		}
	}

	rootPassword, err := randomHex(24)
	if err != nil {
		return cfg, err
	}
	password, err := randomHex(24)
	if err != nil {
		return cfg, err
	}
	cfg = mysqlConfig{RootPassword: rootPassword, Password: password}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return cfg, err
	}
	content := strings.Join([]string{
		"MYSQL_ROOT_PASSWORD=" + cfg.RootPassword,
		"MYSQL_DATABASE=" + mysqlDatabase,
		"MYSQL_USER=" + mysqlUser,
		"MYSQL_PASSWORD=" + cfg.Password,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return cfg, err
	}
	log.Printf("[db] generated MySQL credentials at %s", path)
	return cfg, nil
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values, scanner.Err()
}

func randomHex(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func dockerContainerExists() (bool, error) {
	out, err := dockerOutput("ps", "-a", "--filter", "name=^/"+containerName+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == containerName, nil
}

func ensureMySQLContainer(baseDir string, cfg mysqlConfig, exists bool) error {
	if exists {
		running, err := dockerContainerRunning()
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		_, err = dockerOutput("start", containerName)
		return err
	}

	dataDir := filepath.Join(baseDir, mysqlDataDir)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return err
	}
	absDataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}

	args := []string{
		"run", "-d",
		"--name", containerName,
		"--restart", "unless-stopped",
		"-e", "MARIADB_ROOT_PASSWORD=" + cfg.RootPassword,
		"-e", "MARIADB_DATABASE=" + mysqlDatabase,
		"-e", "MARIADB_USER=" + mysqlUser,
		"-e", "MARIADB_PASSWORD=" + cfg.Password,
		"-p", "127.0.0.1:" + mysqlPort + ":3306",
		"-v", absDataDir + ":/var/lib/mysql",
		mysqlImage,
		"--character-set-server=utf8mb4",
		"--collation-server=utf8mb4_unicode_ci",
	}
	_, err = dockerOutput(args...)
	return err
}

func dockerContainerRunning() (bool, error) {
	out, err := dockerOutput("inspect", "-f", "{{.State.Running}}", containerName)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func dockerOutput(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func openMySQLWithRetry(cfg mysqlConfig, timeout time.Duration) (*gorm.DB, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		db, err := gorm.Open(mysql.Open(dsn(cfg)), &gorm.Config{})
		if err == nil {
			sqlDB, dbErr := db.DB()
			if dbErr == nil {
				if pingErr := sqlDB.Ping(); pingErr == nil {
					return db, nil
				} else {
					lastErr = pingErr
				}
				_ = sqlDB.Close()
			} else {
				lastErr = dbErr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("mysql did not become ready: %w", lastErr)
		}
		time.Sleep(2 * time.Second)
	}
}

func dsn(cfg mysqlConfig) string {
	return fmt.Sprintf("%s:%s@tcp(127.0.0.1:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local&timeout=10s&readTimeout=60s&writeTimeout=60s",
		mysqlUser,
		cfg.Password,
		mysqlPort,
		mysqlDatabase,
	)
}

func configurePool(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err != nil {
		return
	}
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxLifetime(time.Hour)
}

func migrateSQLiteIfNeeded(mysqlDB *gorm.DB, sqlitePath string) error {
	if fileExists(migrationStamp) {
		return nil
	}
	if !targetLooksEmpty(mysqlDB) {
		log.Printf("[db] MySQL already has panel data; skipping SQLite migration")
		return nil
	}

	log.Printf("[db] migrating SQLite data from %s to MySQL", sqlitePath)
	sqliteDB, err := gorm.Open(sqlite.Open(sqlitePath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return err
	}
	sqliteSQL, err := sqliteDB.DB()
	if err == nil {
		defer sqliteSQL.Close()
	}
	if err := sqliteDB.AutoMigrate(models.AllModels()...); err != nil {
		return err
	}

	if err := mysqlDB.Transaction(func(tx *gorm.DB) error {
		if err := setForeignKeyChecks(tx, false); err != nil {
			return err
		}
		defer setForeignKeyChecks(tx, true)
		return copyAllModels(sqliteDB, tx)
	}); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(migrationStamp), 0755); err != nil {
		return err
	}
	stamp := fmt.Sprintf("migrated_at=%s\nsource=%s\n", time.Now().Format(time.RFC3339), sqlitePath)
	if err := os.WriteFile(migrationStamp, []byte(stamp), 0644); err != nil {
		return err
	}
	log.Printf("[db] SQLite migration completed; backup kept at %s", sqlitePath)
	return nil
}

func targetLooksEmpty(db *gorm.DB) bool {
	var count int64
	if err := db.Model(&models.AdminCredential{}).Count(&count).Error; err == nil && count > 0 {
		return false
	}
	if err := db.Model(&models.Task{}).Count(&count).Error; err == nil && count > 0 {
		return false
	}
	return true
}

func setForeignKeyChecks(db *gorm.DB, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	return db.Exec("SET FOREIGN_KEY_CHECKS = " + value).Error
}

func copyAllModels(src, dst *gorm.DB) error {
	copiers := []func(*gorm.DB, *gorm.DB) error{
		copyModel[models.AWVSServer],
		copyModel[models.SqlmapAgent],
		copyModel[models.PathAgent],
		copyModel[models.Task],
		copyModel[models.TaskPathScan],
		copyModel[models.TaskFinding],
		copyModel[models.DomainSQLMapCache],
		copyModel[models.SQLMapGlobalSearchTask],
		copyModel[models.ProxyAgent],
		copyModel[models.CloudSettings],
		copyModel[models.CloudInstance],
		copyModel[models.AdminCredential],
		copyModel[models.AdminSession],
	}
	for _, copier := range copiers {
		if err := copier(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyModel[T any](src, dst *gorm.DB) error {
	var rows []T
	if err := src.Unscoped().Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return dst.CreateInBatches(rows, 500).Error
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
