package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"debian-updater/internal/auth"
	"debian-updater/internal/installationlock"
	"golang.org/x/term"
)

const recoveryUsage = "Usage: webserver admin reset-password [--username ACCOUNT] [--password-stdin] [--factors password|mfa|password,mfa]\nStop the server first. Password and confirmation are read without echo from the terminal.\n--password-stdin requires --username and reads exactly two newline-terminated password lines.\n"

type recoveryOptions struct {
	username string
	stdin    bool
	factors  string
}

func parseRecoveryOptions(args []string) (recoveryOptions, error) {
	options := recoveryOptions{factors: "password"}
	if len(args) < 2 || args[0] != "admin" || args[1] != "reset-password" {
		return options, errors.New("unknown command; use webserver admin reset-password --help")
	}
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--username":
			if options.username != "" || i+1 >= len(args) {
				return options, errors.New("specify one account with --username")
			}
			i++
			options.username = args[i]
		case "--factors":
			if i+1 >= len(args) {
				return options, errors.New("--factors requires password, mfa, or password,mfa")
			}
			i++
			options.factors = args[i]
			if options.factors != "password" && options.factors != "mfa" && options.factors != "password,mfa" {
				return options, errors.New("invalid recovery factors")
			}
		case "--password-stdin":
			if options.stdin {
				return options, errors.New("duplicate --password-stdin")
			}
			options.stdin = true
		default:
			return options, errors.New("unsupported recovery option; passwords must never be supplied as arguments")
		}
	}
	if options.stdin && options.username == "" {
		return options, errors.New("--password-stdin requires an explicit --username")
	}
	if options.username != "" {
		if err := auth.ValidateUsername(options.username); err != nil {
			return options, err
		}
	}
	return options, nil
}

// readRecoveryLine is bounded and avoids buffering subsequent terminal answers.
func readRecoveryLine(input io.Reader, max int) (string, error) {
	line := make([]byte, 0, max)
	var one [1]byte
	for {
		n, err := input.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				value := strings.TrimSuffix(string(line), "\r")
				if len(value) > max {
					return "", errors.New("recovery input is too long")
				}
				return value, nil
			}
			if len(line) >= max+1 {
				return "", errors.New("recovery input is too long")
			}
			line = append(line, one[0])
		}
		if err != nil {
			return "", errors.New("recovery input ended before a complete line")
		}
	}
}

func runAdminCommand(args []string, input *os.File, output io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if (len(args) == 1 && args[0] == "--help") || (len(args) == 2 && args[0] == "admin" && args[1] == "--help") || (len(args) == 3 && args[0] == "admin" && args[1] == "reset-password" && args[2] == "--help") {
		_, err := io.WriteString(output, recoveryUsage)
		return true, err
	}
	options, err := parseRecoveryOptions(args)
	if err != nil {
		return true, err
	}
	if !options.stdin && !term.IsTerminal(int(input.Fd())) {
		return true, errors.New("interactive recovery requires a terminal; automation must explicitly use --password-stdin")
	}
	if options.stdin && term.IsTerminal(int(input.Fd())) {
		return true, errors.New("--password-stdin requires redirected input; omit it for hidden terminal entry")
	}
	var restoreTerminal func()
	if !options.stdin {
		restore, err := guardRecoveryTerminal(input, output)
		if err != nil {
			return true, err
		}
		restoreTerminal = restore
		defer func() {
			if restoreTerminal != nil {
				restoreTerminal()
			}
		}()
	}
	lease, err := installationlock.Acquire(dbPath(), false)
	if err != nil {
		return true, err
	}
	defer lease.Close()
	database, err := openRecoveryDatabase(dbPath())
	if err != nil {
		return true, err
	}
	defer database.Close()
	if options.username == "" {
		if _, err = io.WriteString(output, "Account: "); err != nil {
			return true, err
		}
		options.username, err = readRecoveryLine(input, 64)
		if err != nil {
			return true, err
		}
		if err = auth.ValidateUsername(options.username); err != nil {
			return true, err
		}
	}
	var password, confirmation string
	if options.factors == "mfa" {
		// MFA-only recovery never reads or changes the password.
	} else if options.stdin {
		password, err = readRecoveryLine(input, 256)
		if err == nil {
			confirmation, err = readRecoveryLine(input, 256)
		}
	} else {
		readSecret := func(prompt string) (string, error) {
			if _, err := io.WriteString(output, prompt); err != nil {
				return "", err
			}
			secret, readErr := term.ReadPassword(int(input.Fd()))
			_, writeErr := io.WriteString(output, "\n")
			if readErr != nil {
				return "", errors.New("could not read password from terminal")
			}
			if writeErr != nil {
				return "", writeErr
			}
			value := string(secret)
			clear(secret)
			return value, nil
		}
		password, err = readSecret("New password: ")
		if err == nil {
			confirmation, err = readSecret("Confirm password: ")
		}
	}
	if err != nil {
		return true, err
	}
	if restoreTerminal != nil {
		restoreTerminal()
		restoreTerminal = nil
	}
	result, err := auth.ResetLocalFactors(context.Background(), database, options.username, password, confirmation, strings.Contains(options.factors, "password"), strings.Contains(options.factors, "mfa"))
	if err != nil {
		return true, err
	}
	_, err = fmt.Fprintf(output, "Local account recovery committed. %d session(s) revoked. Restart the server and sign in.\n", result.RevokedSessions)
	if err != nil {
		return true, errors.New("local recovery committed, but confirmation could not be written; restart and verify access")
	}
	return true, nil
}

// ReadPassword restores terminal settings on normal return. Catch termination as
// well so cancelling a recovery cannot leave the operator's terminal without echo.
func guardRecoveryTerminal(input *os.File, output io.Writer) (func(), error) {
	state, err := term.GetState(int(input.Fd()))
	if err != nil {
		return nil, errors.New("could not inspect recovery terminal")
	}
	interrupts := make(chan os.Signal, 1)
	done := make(chan struct{})
	stopped := make(chan struct{})
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer close(stopped)
		select {
		case <-interrupts:
			flushRecoveryInput(input)
			_ = term.Restore(int(input.Fd()), state)
			_, _ = io.WriteString(output, "\nRecovery cancelled.\n")
			os.Exit(130)
		case <-done:
		}
	}()
	return func() { signal.Stop(interrupts); close(done); <-stopped; _ = term.Restore(int(input.Fd()), state) }, nil
}

func openRecoveryDatabase(path string) (*sql.DB, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("recovery requires an existing accessible regular database; no database was created")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid database path")
	}
	database, err := sql.Open("sqlite", recoveryDatabaseDSN(abs))
	if err != nil {
		return nil, errors.New("database could not be opened for recovery")
	}
	database.SetMaxOpenConns(1)
	var integrity string
	if err = database.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		_ = database.Close()
		return nil, errors.New("database is invalid or inaccessible; recovery was not performed")
	}
	return database, nil
}

func recoveryDatabaseDSN(absolutePath string) string {
	path := filepath.ToSlash(absolutePath)
	// A Windows drive letter belongs in the URI path, not its authority.
	// Without the leading slash URL.String emits file://C:/..., which SQLite
	// rejects before opening the database.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return u.String()
}
