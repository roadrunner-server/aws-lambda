package main

import (
	"os"
	"strings"
	"testing"
)

func TestConfigureEnvironment(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("LAMBDA_TASK_ROOT", "/opt/lambda")
	t.Setenv("LD_LIBRARY_PATH", "")

	configureEnvironment()

	path := os.Getenv("PATH")
	if !strings.HasSuffix(path, ":/opt/lambda") {
		t.Fatalf("expected PATH to end with lambda task root, got %q", path)
	}

	if got := os.Getenv("LD_LIBRARY_PATH"); got != "./lib:/lib64:/usr/lib64" {
		t.Fatalf("unexpected LD_LIBRARY_PATH, got %q", got)
	}
}
