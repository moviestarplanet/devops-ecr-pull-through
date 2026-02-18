package main

import (
	"testing"
)

func TestNewServer(t *testing.T) {
	t.Run("missing account ID", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "")
		t.Setenv("ECR_AWS_REGION", "us-east-1")
		_, err := newServer()
		if err == nil {
			t.Fatal("expected error for missing ECR_AWS_ACCOUNT_ID")
		}
	})

	t.Run("missing region", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "123456")
		t.Setenv("ECR_AWS_REGION", "")
		_, err := newServer()
		if err == nil {
			t.Fatal("expected error for missing ECR_AWS_REGION")
		}
	})

	t.Run("builds ECR hostname from account and region", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "123456")
		t.Setenv("ECR_AWS_REGION", "us-east-1")
		t.Setenv("ECR_REGISTRIES", "")
		srv, err := newServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "123456.dkr.ecr.us-east-1.amazonaws.com/"
		if srv.ecrRegistryHostname != want {
			t.Fatalf("ecrRegistryHostname = %q, want %q", srv.ecrRegistryHostname, want)
		}
	})

	t.Run("defaults to docker.io when ECR_REGISTRIES unset", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "123456")
		t.Setenv("ECR_AWS_REGION", "us-east-1")
		t.Setenv("ECR_REGISTRIES", "")
		srv, err := newServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := srv.registries
		if len(got) != 1 || got[0] != "docker.io/" {
			t.Fatalf("expected [docker.io/], got %v", got)
		}
	})

	t.Run("parses registries with trailing slash normalization", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "123456")
		t.Setenv("ECR_AWS_REGION", "us-east-1")
		t.Setenv("ECR_REGISTRIES", "ghcr.io, docker.io/,quay.io")
		srv, err := newServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"ghcr.io/", "docker.io/", "quay.io/"}
		got := srv.registries
		if len(got) != len(want) {
			t.Fatalf("expected %v, got %v", want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("registry[%d]: got %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("filters empty entries", func(t *testing.T) {
		t.Setenv("ECR_AWS_ACCOUNT_ID", "123456")
		t.Setenv("ECR_AWS_REGION", "us-east-1")
		t.Setenv("ECR_REGISTRIES", "ghcr.io,,, docker.io ,")
		srv, err := newServer()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := srv.registries
		if len(got) != 2 {
			t.Fatalf("expected 2 registries, got %v", got)
		}
	})
}
