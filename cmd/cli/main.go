// Command cli is the operator tool for account bootstrap and recovery.
//
// It is deliberately not an HTTP client and never starts a process: every
// operation is a fixed method on the auth service, and user input only ever
// reaches parameterised SQL. There is no shell, no command string, and no
// eval-like path, so an email such as `a@b; rm -rf /` is rejected as an
// invalid address rather than interpreted.
//
// Secrets are read from a hidden terminal prompt, or from stdin with
// --password-stdin for automation. There is no --password flag: arguments are
// visible in shell history and process listings.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/config"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

const usage = `MatchUp operator CLI

Usage:
  cli createsuperuser [--email ADDRESS] [--full-name NAME] [--phone NUMBER] [--password-stdin]
  cli reset-password [--email ADDRESS] [--yes] [--password-stdin]
  cli promote [--email ADDRESS] [--yes]
  cli demote  [--email ADDRESS] [--yes]

Commands:
  createsuperuser  Create an active, email-verified admin account.
  reset-password   Reset an admin password and revoke their sessions. Players
                   use the in-app reset flow.
  promote          Change a player account to admin.
  demote           Change an admin account to player. The last active admin
                   cannot be demoted.

Flags:
  --email           Account address. Prompted when omitted.
  --full-name       Display name. Prompted for createsuperuser.
  --phone           Ethiopian mobile number. Prompted for createsuperuser.
                    Accepted as 09..., 07..., +2519..., +2517... and stored
                    canonically as +251....
  --password-stdin  Read one password line from stdin instead of prompting.
                    Use this in automation; never pass a password as an argument.
  --yes             Skip the confirmation prompt (required when stdin is not a
                    terminal) for reset-password, promote and demote.

Passwords are never echoed, logged, or accepted as command-line arguments.
Admin passwords require at least 10 characters.
`

type command string

const (
	cmdCreateSuper command = "createsuperuser"
	cmdReset       command = "reset-password"
	cmdPromote     command = "promote"
	cmdDemote      command = "demote"
)

type request struct {
	cmd           command
	email         string
	fullName      string
	phone         string
	passwordStdin bool
	yes           bool
	help          bool
}

// accountService is the slice of auth.Service the CLI uses. Keeping it an
// interface means the command plumbing and its safety checks can be tested
// without a database.
type accountService interface {
	CreateAccount(context.Context, auth.CreateAccountInput) (*db.User, error)
	SetAdminPasswordByEmail(context.Context, string, string) (*db.User, error)
	SetRoleByEmail(context.Context, string, string) (*db.User, bool, error)
}

type auditor interface {
	RecordDetached(context.Context, audit.Entry)
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run parses the command before touching configuration or the database, so a
// typo or an injected argument is rejected before the CLI can write anything.
// It may still connect for a command whose values fail validation later; the
// connection is read-only until a service call runs.
func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	req, err := parse(args, errOut)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n\n%s", err, usage)
		return 2
	}
	if req.help {
		fmt.Fprint(out, usage)
		return 0
	}

	config.LoadDotEnv()
	cfg, err := config.Load(config.NeedCLI)
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	store, err := db.Open(ctx, db.PoolConfig{
		URL:             cfg.DatabaseURL,
		MaxConns:        cfg.DBMaxConns,
		MinConns:        cfg.DBMinConns,
		MaxConnLifetime: cfg.DBMaxConnLife,
		MaxConnIdleTime: cfg.DBMaxConnIdle,
	})
	if err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 1
	}
	defer store.Close()

	log := cfg.NewLogger("cli")
	svc := auth.NewService(auth.Options{Store: store, Logger: log})
	return execute(ctx, req, in, out, errOut, svc, audit.NewRecorder(store, log))
}

func parse(args []string, errOut io.Writer) (*request, error) {
	if len(args) == 0 {
		return &request{help: true}, nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		return &request{help: true}, nil
	}

	req := &request{cmd: command(args[0])}
	fs := flag.NewFlagSet(string(req.cmd), flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {}

	switch req.cmd {
	case cmdCreateSuper:
		fs.StringVar(&req.email, "email", "", "")
		fs.StringVar(&req.fullName, "full-name", "", "")
		fs.StringVar(&req.phone, "phone", "", "")
		fs.BoolVar(&req.passwordStdin, "password-stdin", false, "")
	case cmdReset:
		fs.StringVar(&req.email, "email", "", "")
		fs.BoolVar(&req.yes, "yes", false, "")
		fs.BoolVar(&req.passwordStdin, "password-stdin", false, "")
	case cmdPromote, cmdDemote:
		fs.StringVar(&req.email, "email", "", "")
		fs.BoolVar(&req.yes, "yes", false, "")
	default:
		return nil, fmt.Errorf("unknown command %q", args[0])
	}

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &request{help: true}, nil
		}
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return req, nil
}

const maxServiceAttempts = 3

func execute(ctx context.Context, req *request, in io.Reader, out, errOut io.Writer, svc accountService, rec auditor) int {
	return executeWith(ctx, req, in, out, errOut, svc, rec, newPrompter(in, out))
}

func executeWith(ctx context.Context, req *request, in io.Reader, out, errOut io.Writer, svc accountService, rec auditor, p *prompter) int {
	switch req.cmd {
	case cmdCreateSuper:
		var password string
		attempts := 0
		for {
			ident, err := p.identity(req, errOut)
			if err != nil {
				return fail(errOut, err, 2)
			}
			// Keep the accepted values for the next pass, so a retry only
			// asks for the field that failed.
			req.email, req.fullName, req.phone = ident.email, ident.fullName, ident.phone

			if password == "" {
				password, err = p.password(req.passwordStdin, true)
				if err != nil {
					return fail(errOut, err, 2)
				}
			}

			user, err := svc.CreateAccount(ctx, auth.CreateAccountInput{
				FullName:          ident.fullName,
				Email:             ident.email,
				Password:          password,
				Phone:             ident.phone,
				Role:              auth.RoleAdmin,
				MarkEmailVerified: true,
			})
			if err == nil {
				record(ctx, rec, audit.Entry{
					Action:     audit.ActionUserCreated,
					TargetType: audit.TargetUser,
					TargetID:   user.ID.String(),
					Reason:     "created by operator CLI",
					Metadata: map[string]any{
						"email":  user.Email,
						"role":   user.Role,
						"source": "cli",
					},
				})
				fmt.Fprintf(out, "created admin %s (%q)\n", user.ID, user.Email)
				return 0
			}

			apiErr := asAPIError(err)
			if !p.tty || apiErr == nil {
				return fail(errOut, err, 1)
			}
			switch {
			case apiErr.Code == httpx.CodeEmailInUse:
				printError(errOut, err)
				attempts++
				if attempts >= maxServiceAttempts {
					return fail(errOut, err, 1)
				}
				req.email = ""
			case apiErr.Fields["password"] != "":
				printError(errOut, err)
				password = ""
			default:
				return fail(errOut, err, 1)
			}
		}

	case cmdReset:
		attempts := 0
		for {
			email, err := p.emailValue(req.email, errOut)
			if err != nil {
				return fail(errOut, err, 2)
			}
			if ok, err := p.confirm(fmt.Sprintf("Reset the admin password for %q?", email), req.yes); err != nil {
				return fail(errOut, err, 1)
			} else if !ok {
				fmt.Fprintln(out, "aborted")
				return 1
			}

			password := ""
			for {
				if password == "" {
					if password, err = p.password(req.passwordStdin, true); err != nil {
						return fail(errOut, err, 2)
					}
				}

				user, err := svc.SetAdminPasswordByEmail(ctx, email, password)
				if err == nil {
					record(ctx, rec, audit.Entry{
						Action:     audit.ActionUserPasswordReset,
						TargetType: audit.TargetUser,
						TargetID:   user.ID.String(),
						Reason:     "password reset by operator CLI",
						Metadata:   map[string]any{"email": user.Email, "source": "cli"},
					})
					fmt.Fprintf(out, "password reset for %s (%q); all sessions revoked\n", user.ID, user.Email)
					return 0
				}

				apiErr := asAPIError(err)
				if !p.tty || apiErr == nil {
					return fail(errOut, err, 1)
				}
				if apiErr.Fields["password"] != "" {
					printError(errOut, err)
					password = ""
					continue
				}
				if !isLookupError(apiErr) {
					return fail(errOut, err, 1)
				}
				printError(errOut, err)
				attempts++
				if attempts >= maxServiceAttempts {
					return fail(errOut, err, 1)
				}
				req.email = ""
				break
			}
		}

	case cmdPromote, cmdDemote:
		to, from := auth.RoleAdmin, auth.RolePlayer
		verb := "Promote"
		if req.cmd == cmdDemote {
			to, from = auth.RolePlayer, auth.RoleAdmin
			verb = "Demote"
		}
		attempts := 0
		for {
			email, err := p.emailValue(req.email, errOut)
			if err != nil {
				return fail(errOut, err, 2)
			}
			if ok, err := p.confirm(fmt.Sprintf("%s %q (%s -> %s)?", verb, email, from, to), req.yes); err != nil {
				return fail(errOut, err, 1)
			} else if !ok {
				fmt.Fprintln(out, "aborted")
				return 1
			}

			user, changed, err := svc.SetRoleByEmail(ctx, email, to)
			if err == nil {
				if !changed {
					fmt.Fprintf(out, "no change: %s (%q) is already %s\n", user.ID, user.Email, user.Role)
					return 0
				}
				record(ctx, rec, audit.Entry{
					Action:     audit.ActionUserRoleChanged,
					TargetType: audit.TargetUser,
					TargetID:   user.ID.String(),
					Reason:     "role change by operator CLI",
					Metadata: map[string]any{
						"email":  user.Email,
						"from":   from,
						"to":     user.Role,
						"source": "cli",
					},
				})
				fmt.Fprintf(out, "role changed for %s (%q): %s -> %s\n", user.ID, user.Email, from, user.Role)
				return 0
			}

			apiErr := asAPIError(err)
			if !p.tty || apiErr == nil || apiErr.Code != httpx.CodeNotFound {
				return fail(errOut, err, 1)
			}
			printError(errOut, err)
			attempts++
			if attempts >= maxServiceAttempts {
				return fail(errOut, err, 1)
			}
			req.email = ""
		}
	}

	return fail(errOut, errors.New("unknown command"), 2)
}

type identity struct {
	email    string
	fullName string
	phone    string
}

// identity gathers and validates the createsuperuser fields, re-asking for the
// failing field rather than restarting the command. Validation uses the same
// helpers as the HTTP signup handler, so the CLI cannot create an account the
// API would have rejected.
func (p *prompter) identity(req *request, errOut io.Writer) (identity, error) {
	email, err := p.text("Email: ", req.email, "--email", errOut, validateEmailValue)
	if err != nil {
		return identity{}, err
	}
	fullName, err := p.text("Full name: ", req.fullName, "--full-name", errOut, validateFullName)
	if err != nil {
		return identity{}, err
	}
	phone, err := p.text("Phone: ", req.phone, "--phone", errOut, validatePhoneValue)
	if err != nil {
		return identity{}, err
	}
	return identity{email: email, fullName: fullName, phone: phone}, nil
}

// text prompts until the value passes validation. A terminal user keeps the
// valid fields and re-enters only this one; a script fails fast instead of
// looping on input nobody can change.
func (p *prompter) text(label, initial, flag string, errOut io.Writer, validate func(string) (string, error)) (string, error) {
	value := initial
	for {
		if strings.TrimSpace(value) == "" {
			if !p.tty {
				return "", fmt.Errorf("%s is required when stdin is not a terminal", flag)
			}
			line, err := p.line(label)
			if err != nil {
				return "", err
			}
			value = line
		}
		normalized, err := validate(value)
		if err == nil {
			return normalized, nil
		}
		printError(errOut, err)
		if !p.tty {
			return "", err
		}
		value = ""
	}
}

func (p *prompter) emailValue(initial string, errOut io.Writer) (string, error) {
	return p.text("Email: ", initial, "--email", errOut, validateEmailValue)
}

func validateEmailValue(raw string) (string, error) {
	v := httpx.NewValidator()
	email := v.Email("email", raw)
	if len(email) > 254 {
		v.Add("email", "must be at most 254 characters")
	}
	if err := v.Err(); err != nil {
		return "", err
	}
	return email, nil
}

func validateFullName(raw string) (string, error) {
	v := httpx.NewValidator()
	name := v.Required("full_name", raw)
	v.Length("full_name", name, 2, 120)
	if err := v.Err(); err != nil {
		return "", err
	}
	return name, nil
}

func validatePhoneValue(raw string) (string, error) {
	v := httpx.NewValidator()
	phone := v.Required("phone", raw)
	phone = v.Phone("phone", phone)
	if err := v.Err(); err != nil {
		return "", err
	}
	return phone, nil
}

func asAPIError(err error) *httpx.Error {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// isLookupError is the retryable kind: the address was mistyped or names an
// account the CLI does not manage. A different email can fix both.
func isLookupError(err *httpx.Error) bool {
	return err.Code == httpx.CodeNotFound || err.Code == httpx.CodeForbidden
}

type prompter struct {
	in  io.Reader
	br  *bufio.Reader
	out io.Writer
	tty bool
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	tty := false
	if f, ok := in.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &prompter{in: in, br: bufio.NewReaderSize(in, 4096), out: out, tty: tty}
}

func (p *prompter) line(label string) (string, error) {
	if !p.tty {
		return "", errors.New("stdin is not a terminal")
	}
	fmt.Fprint(p.out, label)
	line, err := p.br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" && errors.Is(err, io.EOF) {
		return "", errors.New("end of input")
	}
	if len(line) > 1024 {
		return "", errors.New("input is too long")
	}
	return line, nil
}

// confirm returns false without asking when the input is not a terminal and
// --yes was not passed: an unattended script must state its intent explicitly.
// A terminal user who types something else is asked again instead of having the
// whole command thrown away.
func (p *prompter) confirm(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !p.tty {
		return false, errors.New("confirmation required when stdin is not a terminal; pass --yes")
	}
	for attempt := 0; attempt < maxServiceAttempts; attempt++ {
		fmt.Fprintf(p.out, "%s Type yes or no: ", question)
		line, err := p.br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			if errors.Is(err, io.EOF) {
				return false, nil
			}
		}
		fmt.Fprintln(p.out, "Please answer yes or no.")
	}
	return false, errors.New("no valid confirmation after 3 attempts")
}

// password reads a secret from the terminal, or one line from stdin when the
// caller passed --password-stdin. It never accepts a password argument.
func (p *prompter) password(stdinFlag, confirm bool) (string, error) {
	if stdinFlag {
		if p.tty {
			return "", errors.New("--password-stdin requires piped input; omit it to use the hidden prompt")
		}
		return readPasswordStdin(p.in)
	}
	if !p.tty {
		return "", errors.New("stdin is not a terminal; use --password-stdin")
	}
	f, ok := p.in.(*os.File)
	if !ok {
		return "", errors.New("cannot prompt for a password on this input")
	}

	readSecret := func(label string) ([]byte, error) {
		fmt.Fprint(p.out, label)
		secret, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(p.out)
		if err != nil {
			return nil, err
		}
		return secret, nil
	}

	for attempt := 0; attempt < maxServiceAttempts; attempt++ {
		first, err := readSecret("Password: ")
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if len(first) == 0 {
			fmt.Fprintln(p.out, "Password must not be empty. Try again.")
			continue
		}
		if !confirm {
			return string(first), nil
		}
		second, err := readSecret("Password (again): ")
		if err != nil {
			return "", fmt.Errorf("read password confirmation: %w", err)
		}
		if string(first) == string(second) {
			return string(first), nil
		}
		fmt.Fprintln(p.out, "Passwords do not match. Try again.")
	}
	return "", errors.New("password was not confirmed after 3 attempts")
}

const maxPasswordInput = 4096

// readPasswordStdin takes exactly one line. A newline inside the value is
// rejected rather than silently truncating, so a mistaken multi-line paste
// cannot set a password different from the one the operator typed.
func readPasswordStdin(in io.Reader) (string, error) {
	buf, err := io.ReadAll(io.LimitReader(in, maxPasswordInput+1))
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	if len(buf) > maxPasswordInput {
		return "", fmt.Errorf("password input exceeds %d bytes", maxPasswordInput)
	}
	value := strings.TrimSuffix(string(buf), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" {
		return "", errors.New("password must not be empty")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("password must be a single line without NUL bytes")
	}
	return value, nil
}

func record(ctx context.Context, rec auditor, entry audit.Entry) {
	if rec == nil {
		return
	}
	rec.RecordDetached(ctx, entry)
}

func fail(w io.Writer, err error, code int) int {
	printError(w, err)
	return code
}

func printError(w io.Writer, err error) {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		fmt.Fprintf(w, "error: %s (%s)\n", apiErr.Message, apiErr.Code)
		if len(apiErr.Fields) > 0 {
			keys := make([]string, 0, len(apiErr.Fields))
			for k := range apiErr.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "  %s: %s\n", k, apiErr.Fields[k])
			}
		}
		return
	}
	fmt.Fprintf(w, "error: %v\n", err)
}

// Ensure the compile-time signature of the real recorder matches what the CLI
// expects; a drifting method would otherwise fail only at runtime wiring.
var _ auditor = (*audit.Recorder)(nil)
