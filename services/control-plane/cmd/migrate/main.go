// Package main provides a CLI tool for managing database migrations.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/concord/control-plane/pkg/config"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/joho/godotenv/autoload"
	_ "github.com/lib/pq"
)

func main() {
	var (
		migrationsPath = flag.String("path", "migrations", "Path to migrations directory")
		steps          = flag.Int("steps", 0, "Number of migrations to run/rollback (0 = all)")
		command        = flag.String("command", "", "Migration command: up, down, force, version, create")
		migrationName  = flag.String("name", "", "Name for new migration (used with 'create' command)")
		forceVersion   = flag.Int("force-version", -1, "Version to force (used with 'force' command)")
	)
	flag.Parse()

	if *command == "" {
		fmt.Println("Usage: migrate -command=<up|down|force|version|create> [options]")
		fmt.Println("\nCommands:")
		fmt.Println("  up      - Apply all pending migrations (or use -steps=N for specific count)")
		fmt.Println("  down    - Rollback migrations (use -steps=N, default=1)")
		fmt.Println("  force   - Force migration version (fixes dirty state). Use -force-version=N")
		fmt.Println("  version - Show current migration version")
		fmt.Println("  create  - Create new migration files (requires -name)")
		fmt.Println("\nOptions:")
		fmt.Println("  -path           Path to migrations directory (default: migrations)")
		fmt.Println("  -steps          Number of migrations to apply/rollback")
		fmt.Println("  -force-version  Version number to force (used with 'force' command)")
		fmt.Println("  -name           Name for new migration")
		fmt.Println("\nExamples:")
		fmt.Println("  migrate -command=up")
		fmt.Println("  migrate -command=down -steps=1")
		fmt.Println("  migrate -command=force -force-version=19")
		fmt.Println("  migrate -command=create -name=add_user_status")
		os.Exit(1)
	}

	// Handle 'create' command separately (doesn't need DB connection)
	if *command == "create" {
		if *migrationName == "" {
			log.Fatal("Migration name is required for 'create' command. Use -name flag.")
		}
		if err := createMigration(*migrationsPath, *migrationName); err != nil {
			log.Fatalf("Failed to create migration: %v", err)
		}
		return
	}

	// Load configuration for database connection
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Connect to database
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	// Create migration instance
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		log.Fatalf("Could not create migration driver: %v", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		fmt.Sprintf("file://%s", *migrationsPath),
		"postgres",
		driver,
	)
	if err != nil {
		log.Fatalf("Could not create migrate instance: %v", err)
	}

	// Execute command
	switch *command {
	case "up":
		if *steps > 0 {
			err = m.Steps(*steps)
		} else {
			err = m.Up()
		}
		if err != nil && err != migrate.ErrNoChange {
			log.Fatalf("Migration failed: %v", err)
		}
		if err == migrate.ErrNoChange {
			fmt.Println("No migrations to apply")
		} else {
			fmt.Println("Migrations applied successfully")
		}

	case "down":
		if *steps == 0 {
			*steps = 1 // Default to rolling back 1 migration
		}
		err = m.Steps(-*steps)
		if err != nil && err != migrate.ErrNoChange {
			log.Fatalf("Rollback failed: %v", err)
		}
		if err == migrate.ErrNoChange {
			fmt.Println("No migrations to rollback")
		} else {
			fmt.Printf("Rolled back %d migration(s) successfully\n", *steps)
		}

	case "force":
		if *forceVersion < 0 {
			log.Fatal("Force requires -force-version=N (use -1 to reset to no version)")
		}
		if err := m.Force(*forceVersion); err != nil {
			log.Fatalf("Force failed: %v", err)
		}
		fmt.Printf("Forced migration version to %d (clean)\n", *forceVersion)

	case "version":
		version, dirty, err := m.Version()
		if err != nil && err != migrate.ErrNilVersion {
			log.Fatalf("Could not get version: %v", err)
		}
		if err == migrate.ErrNilVersion {
			fmt.Println("No migrations applied yet")
		} else {
			status := "clean"
			if dirty {
				status = "dirty"
			}
			fmt.Printf("Current version: %d (%s)\n", version, status)
		}

	default:
		log.Fatalf("Unknown command: %s", *command)
	}
}

func createMigration(path, name string) error {
	// Find the next migration number
	files, err := os.ReadDir(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not read migrations directory: %w", err)
	}

	// Create migrations directory if it doesn't exist
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0750); err != nil {
			return fmt.Errorf("could not create migrations directory: %w", err)
		}
	}

	// Determine next version number
	version := 1
	for _, file := range files {
		if !file.IsDir() {
			// Files are named like 000001_name.up.sql
			var v int
			if _, err := fmt.Sscanf(file.Name(), "%d_", &v); err == nil {
				if v >= version {
					version = v + 1
				}
			}
		}
	}

	// Create migration files
	upFile := fmt.Sprintf("%s/%06d_%s.up.sql", path, version, name)
	downFile := fmt.Sprintf("%s/%06d_%s.down.sql", path, version, name)

	// Create up migration file
	upContent := fmt.Sprintf("-- Migration: %s (up)\n-- Created: %s\n\n-- Add your UP migration here\n", name, "auto-generated")
	if err := os.WriteFile(upFile, []byte(upContent), 0600); err != nil {
		return fmt.Errorf("could not create up migration: %w", err)
	}

	// Create down migration file
	downContent := fmt.Sprintf("-- Migration: %s (down)\n-- Created: %s\n\n-- Add your DOWN migration here (rollback)\n", name, "auto-generated")
	if err := os.WriteFile(downFile, []byte(downContent), 0600); err != nil {
		return fmt.Errorf("could not create down migration: %w", err)
	}

	fmt.Printf("Created migration files:\n")
	fmt.Printf("  - %s\n", upFile)
	fmt.Printf("  - %s\n", downFile)

	return nil
}
