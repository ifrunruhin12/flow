package membership

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/bootstrap"
	"github.com/ifrunruhin12/flow/backend/internal/credentials"
	"github.com/ifrunruhin12/flow/backend/internal/database"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const membershipTestSchema = "flow_membership_test"

var errSimulatedUncertainCreate = errors.New("simulated uncertain Navidrome create result")

type retryDirectory struct {
	mu            sync.Mutex
	pool          *pgxpool.Pool
	keyring       *credentials.Keyring
	createCalls   int
	linkCalls     int
	deleteCalls   int
	firstPassword string
}

func (d *retryDirectory) EnsureMemberIdentity(ctx context.Context, _ navidrome.AdminCredential, input navidrome.MemberInput) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.createCalls++
	var state string
	var ciphertext []byte
	var keyVersion int
	if err := d.pool.QueryRow(ctx, `
		SELECT provisioning_state, navidrome_password_enc, navidrome_cred_key_version
		FROM users WHERE username=$1`, input.Username).Scan(&state, &ciphertext, &keyVersion); err != nil {
		return "", err
	}
	if state != "creating_nd_user" || len(ciphertext) == 0 {
		return "", errors.New("remote creation was called before durable credential state committed")
	}
	plaintext, err := d.keyring.Decrypt(input.Username, credentialPurpose, credentials.Envelope{KeyVersion: keyVersion, Ciphertext: ciphertext})
	if err != nil {
		return "", err
	}
	if string(plaintext) != input.Password {
		return "", errors.New("remote credential differs from durable ciphertext")
	}
	if d.firstPassword == "" {
		d.firstPassword = input.Password
		return "", errSimulatedUncertainCreate
	}
	if input.Password != d.firstPassword {
		return "", errors.New("provisioning retry generated a different credential")
	}
	return "nav-member-1", nil
}

func (d *retryDirectory) AssignAllLibraries(context.Context, navidrome.AdminCredential, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.linkCalls++
	return nil
}

func (*retryDirectory) ResolveMemberIdentity(context.Context, navidrome.AdminCredential, string, string) (string, bool, error) {
	return "nav-member-1", true, nil
}

func (d *retryDirectory) DeleteMember(context.Context, navidrome.AdminCredential, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleteCalls++
	return nil
}

func TestRequestApprovalProvisioningLoginPasswordChangeAndSuspend(t *testing.T) {
	databaseURL := os.Getenv("FLOW_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("FLOW_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	basePool, err := database.Open(ctx, databaseURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := basePool.Exec(ctx, `DROP SCHEMA IF EXISTS `+membershipTestSchema+` CASCADE`); err != nil {
		basePool.Close()
		t.Fatal(err)
	}
	if _, err := basePool.Exec(ctx, `CREATE SCHEMA `+membershipTestSchema); err != nil {
		basePool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = basePool.Exec(cleanupContext, `DROP SCHEMA IF EXISTS `+membershipTestSchema+` CASCADE`)
		basePool.Close()
	})

	schemaURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := schemaURL.Query()
	query.Set("search_path", membershipTestSchema)
	schemaURL.RawQuery = query.Encode()
	pool, err := database.Open(ctx, schemaURL.String(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.MigrateUp(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}

	keyring, err := credentials.New(1, map[int][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService := bootstrap.New(pool, keyring)
	if err := bootstrapService.PrepareKeyVersions(ctx); err != nil {
		t.Fatal(err)
	}
	adminID, err := bootstrapService.EnsureAdmin(ctx, "flow-admin", "flow-admin-password", "navidrome-admin-password", navidrome.Verification{
		UserID: "nav-admin-1", Username: "nav-admin",
	}, "test-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	storedAdminUsername, storedAdminPassword, err := bootstrapService.DecryptAdminCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if storedAdminUsername != "nav-admin" || storedAdminPassword != "navidrome-admin-password" {
		t.Fatal("stored Navidrome administrator credential did not preserve its separate identity")
	}
	authService, err := auth.NewService(pool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	adminLogin, err := authService.Login(ctx, "flow-admin", "flow-admin-password", "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	rotatedAdminID, err := bootstrapService.EnsureAdmin(ctx, "flow-admin", "flow-admin-password-rotated", "navidrome-admin-password", navidrome.Verification{
		UserID: "nav-admin-1", Username: "nav-admin",
	}, "test-bootstrap-rotation")
	if err != nil || rotatedAdminID != adminID {
		t.Fatalf("rotated admin id=%d error=%v", rotatedAdminID, err)
	}
	if _, err := authService.Authenticate(ctx, adminLogin.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("rotated administrator session error = %v", err)
	}
	if _, err := authService.Login(ctx, "flow-admin", "flow-admin-password", "integration-test"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old administrator password error = %v", err)
	}
	if _, err := authService.Login(ctx, "flow-admin", "flow-admin-password-rotated", "integration-test"); err != nil {
		t.Fatal(err)
	}
	rotatedKeyring, err := credentials.New(2, map[int][]byte{
		1: make([]byte, 32),
		2: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService = bootstrap.New(pool, rotatedKeyring)
	if err := bootstrapService.PrepareKeyVersions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapService.ReencryptCredentials(ctx); err != nil {
		t.Fatal(err)
	}
	storedAdminUsername, storedAdminPassword, err = bootstrapService.DecryptAdminCredential(ctx)
	if err != nil || storedAdminUsername != "nav-admin" || storedAdminPassword != "navidrome-admin-password" {
		t.Fatalf("rotated keyring credential username=%q error=%v", storedAdminUsername, err)
	}
	keyring = rotatedKeyring

	directory := &retryDirectory{pool: pool, keyring: keyring}
	service := New(pool, keyring, directory, navidrome.AdminCredential{Username: "nav-admin", Password: "navidrome-admin-password"}, true, 5*time.Second)
	requestID, err := service.RegisterRequest(ctx, "Flow Member", "member@example.test", "flow.member")
	if err != nil {
		t.Fatal(err)
	}
	temporaryPassword := "temporary-member-password"
	created, approvalErr := service.ApproveRequest(ctx, requestID, adminID, temporaryPassword, "approved", "test-approve")
	if approvalErr == nil || created.ID == 0 {
		t.Fatalf("first approval result user=%+v error=%v", created, approvalErr)
	}
	if !errors.Is(approvalErr, errSimulatedUncertainCreate) {
		t.Fatalf("first approval error = %v, want %v", approvalErr, errSimulatedUncertainCreate)
	}
	failed, err := service.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.ProvisioningState != "failed" || failed.ProvisioningFailed == nil || *failed.ProvisioningFailed != "creating_nd_user" {
		t.Fatalf("unexpected failed provisioning state after %v: %+v", approvalErr, failed)
	}
	if _, err := authService.Login(ctx, "flow.member", temporaryPassword, "integration-test"); !errors.Is(err, auth.ErrAccountNotActive) {
		t.Fatalf("partially provisioned login error = %v", err)
	}
	if err := service.Provision(ctx, created.ID, adminID, "test-retry"); err != nil {
		t.Fatal(err)
	}
	complete, err := service.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.ProvisioningState != "complete" || directory.createCalls != 2 || directory.linkCalls != 1 {
		t.Fatalf("unexpected completed provisioning: user=%+v createCalls=%d linkCalls=%d", complete, directory.createCalls, directory.linkCalls)
	}

	login, err := authService.Login(ctx, "flow.member", temporaryPassword, "integration-test")
	if err != nil {
		t.Fatal(err)
	}
	if !login.User.MustChangePassword {
		t.Fatal("temporary-password login was not restricted")
	}
	otherLogin, err := authService.Login(ctx, "flow.member", temporaryPassword, "integration-test-other-session")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authService.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	newPassword := "replacement-member-password"
	if err := authService.ChangePassword(ctx, principal, temporaryPassword, newPassword); err != nil {
		t.Fatal(err)
	}
	currentPrincipal, err := authService.Authenticate(ctx, login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if currentPrincipal.User.MustChangePassword {
		t.Fatal("password replacement did not clear the restricted-session flag")
	}
	if _, err := authService.Authenticate(ctx, otherLogin.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("other session authentication error = %v", err)
	}
	if _, err := authService.Login(ctx, "flow.member", temporaryPassword, "integration-test"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("temporary password login error = %v", err)
	}
	if _, err := authService.Login(ctx, "flow.member", newPassword, "integration-test"); err != nil {
		t.Fatal(err)
	}
	if err := service.Suspend(ctx, created.ID, adminID, "test suspension", "test-suspend"); err != nil {
		t.Fatal(err)
	}
	if _, err := authService.Authenticate(ctx, login.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked session authentication error = %v", err)
	}
	if _, err := authService.Login(ctx, "flow.member", newPassword, "integration-test"); !errors.Is(err, auth.ErrAccountNotActive) {
		t.Fatalf("suspended login error = %v", err)
	}
	if err := service.Delete(ctx, created.ID, adminID, true, "test-delete"); err != nil {
		t.Fatal(err)
	}
	deleted, err := service.GetUser(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != "deleted" || deleted.NavidromeCleanup != "complete" || directory.deleteCalls != 1 {
		t.Fatalf("unexpected deletion state: user=%+v deleteCalls=%d", deleted, directory.deleteCalls)
	}
	uncertainDirectory := &retryDirectory{pool: pool, keyring: keyring}
	uncertainService := New(pool, keyring, uncertainDirectory, navidrome.AdminCredential{Username: "nav-admin", Password: "navidrome-admin-password"}, true, 5*time.Second)
	uncertainUser, err := uncertainService.CreateMember(ctx, adminID, NewMember{
		Username: "uncertain.member", DisplayName: "Uncertain Member", TemporaryPassword: "uncertain-temporary-password",
	}, "test-uncertain-create")
	if err == nil || uncertainUser.ID == 0 {
		t.Fatalf("uncertain creation user=%+v error=%v", uncertainUser, err)
	}
	if err := uncertainService.Delete(ctx, uncertainUser.ID, adminID, true, "test-uncertain-delete"); err != nil {
		t.Fatal(err)
	}
	uncertainDeleted, err := uncertainService.GetUser(ctx, uncertainUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uncertainDeleted.NavidromeCleanup != "complete" || uncertainDirectory.deleteCalls != 1 {
		t.Fatalf("uncertain cleanup user=%+v deleteCalls=%d", uncertainDeleted, uncertainDirectory.deleteCalls)
	}
}
