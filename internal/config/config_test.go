package config

import (
	"strings"
	"testing"
)

func TestValidate_SSLModeProductionGuard(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		databaseURL string
		wantErr     bool
	}{
		{"dev allows disable", "development", "postgres://u:p@h/db?sslmode=disable", false},
		{"prod rejects disable", "production", "postgres://u:p@h/db?sslmode=disable", true},
		{"prod rejects uppercase", "production", "postgres://u:p@h/db?sslmode=DISABLE", true},
		{"prod rejects allow", "production", "postgres://u:p@h/db?sslmode=allow", true},
		{"prod rejects prefer", "production", "postgres://u:p@h/db?sslmode=prefer", true},
		{"prod accepts require", "production", "postgres://u:p@h/db?sslmode=require", false},
		{"prod accepts verify-full", "production", "postgres://u:p@h/db?sslmode=verify-full", false},
		{"prod ignores sslmode inside password", "production", "postgres://u:sslmode%3Ddisable@h/db?sslmode=require", false},
		{"prod rejects keyword form disable", "production", "host=h user=u password=p dbname=db sslmode=disable", true},
		{"prod accepts keyword form require", "production", "host=h user=u password=p dbname=db sslmode=require", false},
		{"prod env comparison is case-insensitive", "PRODUCTION", "postgres://u:p@h/db?sslmode=disable", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{DatabaseURL: tc.databaseURL, Env: tc.env}
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "sslmode") {
				t.Fatalf("expected sslmode error, got: %v", err)
			}
		})
	}
}

func TestValidate_MissingDatabaseURL(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for missing DATABASE_URL")
	}
}
