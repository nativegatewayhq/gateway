//go:build integration

package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nativegatewayhq/gateway/internal/database"
)

func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is required")
	}
	ctx := context.Background()
	admin, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	return pool
}

func seedTenant(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO organizations(id,name,slug) VALUES('org_wallet','Wallet','wallet'); INSERT INTO projects(id,organization_id,name,slug) VALUES('project_wallet','org_wallet','Wallet project','wallet')`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWalletLifecycleIdempotencyAndAudit(t *testing.T) {
	pool := isolatedPool(t)
	seedTenant(t, pool)
	service := NewService(pool)
	ctx := context.Background()
	balance, err := service.Deposit(ctx, "org_wallet", 1000, "deposit-1")
	if err != nil || balance.Available != 1000 {
		t.Fatalf("deposit=%+v %v", balance, err)
	}
	balance, err = service.Deposit(ctx, "org_wallet", 1000, "deposit-1")
	if err != nil || balance.Available != 1000 {
		t.Fatalf("retry=%+v %v", balance, err)
	}
	if _, err := service.Deposit(ctx, "org_wallet", 999, "deposit-1"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict=%v", err)
	}
	reserved, err := service.Reserve(ctx, "org_wallet", "project_wallet", "request-1", 700, "reserve-1")
	if err != nil || reserved.Balance.Available != 300 || reserved.Balance.Reserved != 700 {
		t.Fatalf("reserve=%+v %v", reserved, err)
	}
	retry, err := service.Reserve(ctx, "org_wallet", "project_wallet", "request-1", 700, "reserve-1")
	if err != nil || retry.Reservation.ID != reserved.Reservation.ID {
		t.Fatalf("reserve retry=%+v %v", retry, err)
	}
	captured, err := service.Capture(ctx, reserved.Reservation.ID, 450, "capture-1")
	if err != nil || captured.Balance.Available != 550 || captured.Balance.Reserved != 0 || captured.Reservation.Captured != 450 {
		t.Fatalf("capture=%+v %v", captured, err)
	}
	refunded, err := service.Refund(ctx, reserved.Reservation.ID, 100, "refund-1")
	if err != nil || refunded.Balance.Available != 650 || refunded.Reservation.Refunded != 100 {
		t.Fatalf("refund=%+v %v", refunded, err)
	}
	if _, err := service.Refund(ctx, reserved.Reservation.ID, 351, "refund-too-much"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("excess refund=%v", err)
	}
	var available, reservedSum int64
	if err := pool.QueryRow(ctx, `SELECT coalesce(sum(delta_available),0),coalesce(sum(delta_reserved),0) FROM ledger_entries WHERE organization_id='org_wallet'`).Scan(&available, &reservedSum); err != nil {
		t.Fatal(err)
	}
	if available != 650 || reservedSum != 0 {
		t.Fatalf("ledger sums=%d/%d", available, reservedSum)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET delta_available=1`); err == nil {
		t.Fatal("ledger update succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM ledger_entries`); err == nil {
		t.Fatal("ledger delete succeeded")
	}
}

func TestConcurrentReserveNeverMakesBalanceNegative(t *testing.T) {
	pool := isolatedPool(t)
	seedTenant(t, pool)
	service := NewService(pool)
	ctx := context.Background()
	if _, err := service.Deposit(ctx, "org_wallet", 100, "deposit"); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for i := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.Reserve(ctx, "org_wallet", "project_wallet", fmt.Sprintf("request-%d", i), 80, fmt.Sprintf("reserve-%d", i))
			outcomes <- outcome{result, err}
		}()
	}
	wait.Wait()
	close(outcomes)
	success, insufficient := 0, 0
	for result := range outcomes {
		if result.err == nil {
			success++
		} else if errors.Is(result.err, ErrInsufficientFunds) {
			insufficient++
		} else {
			t.Fatal(result.err)
		}
	}
	if success != 1 || insufficient != 1 {
		t.Fatalf("outcomes=%d/%d", success, insufficient)
	}
	var available, reserved int64
	if err := pool.QueryRow(ctx, `SELECT available,reserved FROM organization_wallets WHERE organization_id='org_wallet'`).Scan(&available, &reserved); err != nil {
		t.Fatal(err)
	}
	if available != 20 || reserved != 80 {
		t.Fatalf("balance=%d/%d", available, reserved)
	}
}

func TestConcurrentIdenticalDepositHasOneEffect(t *testing.T) {
	pool := isolatedPool(t)
	seedTenant(t, pool)
	service := NewService(pool)
	ctx := context.Background()
	errorsChannel := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Deposit(ctx, "org_wallet", 100, "same-deposit")
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	var available, entries int64
	if err := pool.QueryRow(ctx, `SELECT available FROM organization_wallets WHERE organization_id='org_wallet'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE organization_id='org_wallet'`).Scan(&entries); err != nil {
		t.Fatal(err)
	}
	if available != 100 || entries != 1 {
		t.Fatalf("available/entries=%d/%d", available, entries)
	}
}

func TestReleaseAndRejectedCommandsLeaveConsistentProjection(t *testing.T) {
	pool := isolatedPool(t)
	seedTenant(t, pool)
	service := NewService(pool)
	ctx := context.Background()
	if _, err := service.Deposit(ctx, "org_wallet", 500, "deposit"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve(ctx, "org_wallet", "project_wallet", "too-large", 501, "reserve-too-large"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("insufficient funds error=%v", err)
	}
	reserved, err := service.Reserve(ctx, "org_wallet", "project_wallet", "release-request", 300, "reserve-release")
	if err != nil {
		t.Fatal(err)
	}
	released, err := service.Release(ctx, reserved.Reservation.ID, "release")
	if err != nil {
		t.Fatal(err)
	}
	if released.Balance.Available != 500 || released.Balance.Reserved != 0 || released.Reservation.State != "released" {
		t.Fatalf("release=%+v", released)
	}
	retry, err := service.Release(ctx, reserved.Reservation.ID, "release")
	if err != nil || retry != released {
		t.Fatalf("release retry=%+v error=%v", retry, err)
	}
	if _, err := service.Capture(ctx, reserved.Reservation.ID, 1, "capture-after-release"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("capture after release error=%v", err)
	}
	assertProjectionMatchesLedger(t, pool, "org_wallet")
}

func TestCommandsRejectInactiveOrMismatchedTenant(t *testing.T) {
	pool := isolatedPool(t)
	seedTenant(t, pool)
	service := NewService(pool)
	ctx := context.Background()
	if _, err := service.Deposit(ctx, "org_wallet", 100, "deposit"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE projects SET status='disabled' WHERE id='project_wallet'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reserve(ctx, "org_wallet", "project_wallet", "disabled", 10, "disabled"); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("disabled project error=%v", err)
	}
	if _, err := service.Reserve(ctx, "org_other", "project_wallet", "mismatch", 10, "mismatch"); !errors.Is(err, ErrTenantUnavailable) {
		t.Fatalf("mismatched tenant error=%v", err)
	}
	assertProjectionMatchesLedger(t, pool, "org_wallet")
}

func assertProjectionMatchesLedger(t *testing.T, pool *pgxpool.Pool, organizationID string) {
	t.Helper()
	var walletAvailable, walletReserved, ledgerAvailable, ledgerReserved int64
	err := pool.QueryRow(context.Background(), `
		SELECT w.available,w.reserved,coalesce(sum(l.delta_available),0),coalesce(sum(l.delta_reserved),0)
		FROM organization_wallets w LEFT JOIN ledger_entries l ON l.organization_id=w.organization_id
		WHERE w.organization_id=$1 GROUP BY w.available,w.reserved`, organizationID).
		Scan(&walletAvailable, &walletReserved, &ledgerAvailable, &ledgerReserved)
	if err != nil {
		t.Fatal(err)
	}
	if walletAvailable != ledgerAvailable || walletReserved != ledgerReserved {
		t.Fatalf("projection=%d/%d ledger=%d/%d", walletAvailable, walletReserved, ledgerAvailable, ledgerReserved)
	}
}
