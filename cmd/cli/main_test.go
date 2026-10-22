package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Zahir-Seid/MatchUp_backend/internal/audit"
	"github.com/Zahir-Seid/MatchUp_backend/internal/auth"
	"github.com/Zahir-Seid/MatchUp_backend/internal/db"
	"github.com/Zahir-Seid/MatchUp_backend/internal/httpx"
)

type fakeService struct {
	createCalls int
	resetCalls  int
	roleCalls   int
	createIn    auth.CreateAccountInput
	resetEmail  string
	resetPW     string
	roleEmail   string
	role        string
	user        *db.User
	err         error
	roleErrs    []error
}

func (f *fakeService) CreateAccount(_ context.Context, in auth.CreateAccountInput) (*db.User, error) {
	f.createCalls++
	f.createIn = in
	if f.err != nil {
		return nil, f.err
	}
	if f.user != nil {
		return f.user, nil
	}
	return &db.User{ID: uuid.New(), Email: in.Email, Role: in.Role}, nil
}

func (f *fakeService) SetAdminPasswordByEmail(_ context.Context, email, password string) (*db.User, error) {
	f.resetCalls++
	f.resetEmail, f.resetPW = email, password
	if f.err != nil {
		return nil, f.err
	}
	if f.user != nil {
		return f.user, nil
	}
	return &db.User{ID: uuid.New(), Email: email}, nil
}

func (f *fakeService) SetRoleByEmail(_ context.Context, email, role string) (*db.User, bool, error) {
	f.roleCalls++
	f.roleEmail, f.role = email, role
	if len(f.roleErrs) > 0 {
		err := f.roleErrs[0]
		f.roleErrs = f.roleErrs[1:]
		if err != nil {
			return nil, false, err
		}
	}
	if f.err != nil {
		return nil, false, f.err
	}
	if f.user != nil {
		changed := f.user.Role != role
		f.user.Role = role
		return f.user, changed, nil
	}
	return &db.User{ID: uuid.New(), Email: email, Role: role}, true, nil
}

type fakeAuditor struct {
	entries []audit.Entry
}

func (f *fakeAuditor) RecordDetached(_ context.Context, e audit.Entry) {
	f.entries = append(f.entries, e)
}

func TestParseRejectsUnknownCommandAndExtraArguments(t *testing.T) {
	_, err := parse([]string{"drop-database"}, io.Discard)
	require.Error(t, err)

	_, err = parse([]string{"promote", "--email", "a@b.com", "--force"}, io.Discard)
	require.Error(t, err)

	_, err = parse([]string{"promote", "--email", "a@b.com", "extra"}, io.Discard)
	require.Error(t, err)
}

func TestParseHasNoPasswordArgument(t *testing.T) {
	_, err := parse([]string{"createsuperuser", "--password", "hunter2"}, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "password")
}

func TestResetRequiresConfirmationWhenNotATerminal(t *testing.T) {
	svc := &fakeService{}
	rec := &fakeAuditor{}
	in := strings.NewReader("hunter2\n")
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdReset, email: "player@example.com", passwordStdin: true},
		in, &out, &errOut, svc, rec)

	assert.Equal(t, 1, code)
	assert.Zero(t, svc.resetCalls)
	assert.Zero(t, len(rec.entries))
	assert.Contains(t, errOut.String(), "--yes")
}

func TestPromoteWithYesCallsServiceAndAudits(t *testing.T) {
	user := &db.User{ID: uuid.New(), Email: "player@example.com", Role: auth.RolePlayer}
	svc := &fakeService{user: user}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdPromote, email: "Player@Example.com", yes: true},
		strings.NewReader(""), &out, &errOut, svc, rec)

	require.Zero(t, code, errOut.String())
	assert.Equal(t, 1, svc.roleCalls)
	assert.Equal(t, "player@example.com", svc.roleEmail)
	assert.Equal(t, auth.RoleAdmin, svc.role)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, audit.ActionUserRoleChanged, rec.entries[0].Action)
	assert.Equal(t, "cli", rec.entries[0].Metadata["source"])
	assert.Equal(t, auth.RolePlayer, rec.entries[0].Metadata["from"])
	assert.Equal(t, auth.RoleAdmin, rec.entries[0].Metadata["to"])
	assert.Contains(t, out.String(), user.ID.String())
}

func TestPromoteAlreadyAdminWritesNoAudit(t *testing.T) {
	user := &db.User{ID: uuid.New(), Email: "admin@example.com", Role: auth.RoleAdmin}
	svc := &fakeService{user: user}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdPromote, email: "admin@example.com", yes: true},
		strings.NewReader(""), &out, &errOut, svc, rec)

	require.Zero(t, code, errOut.String())
	assert.Zero(t, len(rec.entries))
	assert.Contains(t, out.String(), "no change")
}

func TestDemoteRefusalFromServiceWritesNoAudit(t *testing.T) {
	svc := &fakeService{err: httpx.NewError(409, httpx.CodeConflict, "That is the last active admin; promote another account first.")}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdDemote, email: "admin@example.com", yes: true},
		strings.NewReader(""), &out, &errOut, svc, rec)

	assert.Equal(t, 1, code)
	assert.Equal(t, 1, svc.roleCalls)
	assert.Zero(t, len(rec.entries))
	assert.Contains(t, errOut.String(), "last active admin")
}

func TestCreateSuperuserRejectsInjectedEmailBeforeCallingService(t *testing.T) {
	svc := &fakeService{}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{
			cmd:           cmdCreateSuper,
			email:         "admin@example.com; rm -rf /",
			fullName:      "Admin",
			phone:         "0911223344",
			passwordStdin: true,
		},
		strings.NewReader("hunter2\n"), &out, &errOut, svc, rec)

	assert.Equal(t, 2, code)
	assert.Zero(t, svc.createCalls)
	assert.Zero(t, len(rec.entries))
	assert.Contains(t, errOut.String(), "email")
}

func TestCreateSuperuserMarksVerifiedAndNeverEchoesPassword(t *testing.T) {
	svc := &fakeService{}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{
			cmd:           cmdCreateSuper,
			email:         "Admin@Example.com",
			fullName:      "Admin User",
			phone:         "0911223344",
			passwordStdin: true,
		},
		strings.NewReader("s3cret-pass\n"), &out, &errOut, svc, rec)

	require.Zero(t, code, errOut.String())
	assert.Equal(t, 1, svc.createCalls)
	assert.Equal(t, auth.RoleAdmin, svc.createIn.Role)
	assert.True(t, svc.createIn.MarkEmailVerified)
	assert.Equal(t, "admin@example.com", svc.createIn.Email)
	assert.Equal(t, "s3cret-pass", svc.createIn.Password)
	assert.NotContains(t, out.String(), "s3cret-pass")
	assert.NotContains(t, errOut.String(), "s3cret-pass")
	require.Len(t, rec.entries, 1)
	assert.Equal(t, audit.ActionUserCreated, rec.entries[0].Action)
	assert.NotContains(t, rec.entries[0].Metadata, "password")
}

func TestPasswordStdinRejectsMultipleLines(t *testing.T) {
	_, err := readPasswordStdin(strings.NewReader("first\nsecond\n"))
	require.Error(t, err)
}

func TestPasswordStdinKeepsSpacesAndStripsOneLineEnding(t *testing.T) {
	got, err := readPasswordStdin(strings.NewReader("  pass word  \r\n"))
	require.NoError(t, err)
	assert.Equal(t, "  pass word  ", got)
}

func TestCreateSuperuserRequiresIdentityFlagsWhenNotATerminal(t *testing.T) {
	svc := &fakeService{}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdCreateSuper, passwordStdin: true},
		strings.NewReader("hunter2\n"), &out, &errOut, svc, rec)

	assert.Equal(t, 2, code)
	assert.Zero(t, svc.createCalls)
	assert.Contains(t, errOut.String(), "--email is required")
}

func TestResetWithYesRevokesThroughService(t *testing.T) {
	user := &db.User{ID: uuid.New(), Email: "player@example.com"}
	svc := &fakeService{user: user}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdReset, email: "player@example.com", yes: true, passwordStdin: true},
		strings.NewReader("new-password\n"), &out, &errOut, svc, rec)

	require.Zero(t, code, errOut.String())
	assert.Equal(t, 1, svc.resetCalls)
	assert.Equal(t, "new-password", svc.resetPW)
	require.Len(t, rec.entries, 1)
	assert.Equal(t, audit.ActionUserPasswordReset, rec.entries[0].Action)
	assert.NotContains(t, out.String(), "new-password")
}

func TestServiceErrorReturnsOneAndPrintsMessage(t *testing.T) {
	svc := &fakeService{err: errors.New("connection refused")}
	var out, errOut bytes.Buffer

	code := execute(context.Background(),
		&request{cmd: cmdPromote, email: "a@b.com", yes: true},
		strings.NewReader(""), &out, &errOut, svc, nil)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "connection refused")
}

func TestPromoteRetriesUnknownEmailInTerminal(t *testing.T) {
	user := &db.User{ID: uuid.New(), Email: "right@example.com", Role: auth.RolePlayer}
	svc := &fakeService{
		user: user,
		roleErrs: []error{
			httpx.NewError(404, httpx.CodeNotFound, "No account uses that email address."),
			nil,
		},
	}
	rec := &fakeAuditor{}
	var out, errOut bytes.Buffer
	p := &prompter{
		in:  strings.NewReader("wrong@example.com\nyes\nright@example.com\nyes\n"),
		out: &out,
		tty: true,
	}
	p.br = bufio.NewReader(p.in)

	code := executeWith(context.Background(), &request{cmd: cmdPromote},
		p.in, &out, &errOut, svc, rec, p)

	require.Zero(t, code, errOut.String())
	assert.Equal(t, 2, svc.roleCalls)
	assert.Equal(t, "right@example.com", svc.roleEmail)
	assert.Contains(t, errOut.String(), "No account uses that email")
	require.Len(t, rec.entries, 1)
}

func TestConfirmAcceptsYesNoAndRetriesOtherAnswers(t *testing.T) {
	var out bytes.Buffer
	p := &prompter{in: strings.NewReader("yea\nn\n"), out: &out, tty: true}
	p.br = bufio.NewReader(p.in)

	ok, err := p.confirm("Proceed?", false)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, out.String(), "Please answer yes or no")
}

func TestLookupErrorsAreRetryable(t *testing.T) {
	assert.True(t, isLookupError(httpx.NewError(404, httpx.CodeNotFound, "missing")))
	assert.True(t, isLookupError(httpx.NewError(403, httpx.CodeForbidden, "player")))
	assert.False(t, isLookupError(httpx.NewError(409, httpx.CodeConflict, "last admin")))
	assert.False(t, isLookupError(httpx.NewError(500, httpx.CodeInternal, "boom")))
}
