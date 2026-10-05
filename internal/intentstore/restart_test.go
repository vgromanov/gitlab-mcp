package intentstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestIntentStoreChild(t *testing.T) {
	if os.Getenv("INTENTSTORE_CHILD") != "1" {
		return
	}
	path := os.Getenv("INTENTSTORE_DB")
	mode := os.Getenv("INTENTSTORE_MODE")
	op := os.Getenv("INTENTSTORE_OP")
	result := os.Getenv("INTENTSTORE_RESULT")
	s, err := Open(Config{Path: path})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch mode {
	case "crash-send":
		if _, err := s.ClaimSending(context.Background(), op); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(97)
	case "claim":
		_, err := s.ClaimSending(context.Background(), op)
		msg := "claimed"
		if errors.Is(err, ErrAlreadySending) {
			msg = "busy"
		} else if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.WriteFile(result, []byte(msg), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = s.Close()
	default:
		fmt.Fprintln(os.Stderr, "unknown child mode")
		os.Exit(2)
	}
}

func TestRestartChildCrash(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("restart"), PayloadHash([]byte("restart-payload")), BeginOptions{ExpectedHead: "head"})
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	op := rec.OperationID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := runChild(path, "crash-send", op, "")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 97 {
		t.Fatalf("child exit %v\n%s", err, out)
	}
	s2 := openStore(t, cfg)
	got, err := s2.Get(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSending || got.SendingAt.IsZero() || got.PayloadHash != rec.PayloadHash {
		t.Fatalf("restart receipt %+v", got)
	}
	if _, err := s2.ClaimSending(ctx, op); !errors.Is(err, ErrAlreadySending) {
		t.Fatalf("second dispatch after crash: %v", err)
	}
}

func TestTwoProcessesOneClaim(t *testing.T) {
	cfg, _ := fixedNow(t)
	s := openStore(t, cfg)
	ctx := context.Background()
	rec, err := s.Begin(ctx, ident("race"), PayloadHash([]byte("race-payload")), BeginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := s.path
	op := rec.OperationID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := privateDir(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result := filepath.Join(dir, fmt.Sprintf("r%d", i))
			out, err := runChild(path, "claim", op, result)
			if err != nil {
				errCh <- fmt.Errorf("child %d: %w\n%s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	claimed := 0
	for i := 0; i < 2; i++ {
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("r%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) == "claimed" {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed=%d want 1", claimed)
	}
	s2 := openStore(t, cfg)
	got, err := s2.Get(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateSending {
		t.Fatalf("state %s", got.State)
	}
}

func runChild(dbPath, mode, op, result string) ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "-test.run=^TestIntentStoreChild$", "-test.count=1")
	env := []string{
		"INTENTSTORE_CHILD=1",
		"INTENTSTORE_DB=" + dbPath,
		"INTENTSTORE_MODE=" + mode,
		"INTENTSTORE_OP=" + op,
		"TMPDIR=" + os.TempDir(),
	}
	if result != "" {
		env = append(env, "INTENTSTORE_RESULT="+result)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}
