package main

import (
	"strings"
	"testing"

	"github.com/ibldzn/trs/internal/config"
)

func TestMigrationNamePattern(t *testing.T) {
	for _, name := range []string{"create_users", "add-role", "phase0"} {
		if !migrationNamePattern.MatchString(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	for _, name := range []string{"", "../outside", "spaces are bad", "!invalid"} {
		if migrationNamePattern.MatchString(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestMigrationMutationPolicy(t *testing.T) {
	production := config.Config{App: config.AppConfig{Environment: "production"}, Database: config.DatabaseConfig{Name: "go_admin"}}
	if err := validateCommandPolicy(production, "up", "go_admin"); err != nil {
		t.Fatalf("confirmed production up: %v", err)
	}
	for _, test := range []struct {
		name, command, confirmed string
		config                   config.Config
		want                     string
	}{
		{"missing confirmation", "up", "", production, "--confirm-database go_admin"},
		{"wrong confirmation", "up", "other", production, "--confirm-database go_admin"},
		{"production down", "down", "", production, "down is disabled"},
		{"empty configured database", "up", "", config.Config{App: config.AppConfig{Environment: "development"}}, "configured database name is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateCommandPolicy(test.config, test.command, test.confirmed)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
	if err := validateCommandPolicy(config.Config{App: config.AppConfig{Environment: "development"}, Database: config.DatabaseConfig{Name: "go_admin"}}, "up", ""); err != nil {
		t.Fatalf("development up unexpectedly blocked: %v", err)
	}
}
