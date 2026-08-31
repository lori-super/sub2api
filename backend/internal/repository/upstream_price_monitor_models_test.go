package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSetModelCatalogManagedRejectsIncompleteAccountCoverage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &upstreamPriceMonitorRepository{db: db}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.seen_account_count,m\.expected_account_count`).
		WithArgs("deepseek-new-v1").
		WillReturnRows(sqlmock.NewRows([]string{"seen", "expected", "domestic", "complete"}).AddRow(1, 2, true, true))
	mock.ExpectRollback()

	_, err = repo.SetModelCatalogStatus(
		context.Background(), "deepseek-new-v1", domain.UpstreamPriceModelStatusManaged,
	)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetModelCatalogManagedRejectsForeignModelEvenWithCompleteCoverage(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &upstreamPriceMonitorRepository{db: db}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.seen_account_count,m\.expected_account_count`).
		WithArgs("claude-opus-4-6").
		WillReturnRows(sqlmock.NewRows([]string{"seen", "expected", "domestic", "complete"}).AddRow(2, 2, false, true))
	mock.ExpectRollback()

	_, err = repo.SetModelCatalogStatus(
		context.Background(), "claude-opus-4-6", domain.UpstreamPriceModelStatusManaged,
	)
	require.ErrorIs(t, err, service.ErrUpstreamPriceMonitorInvalidConfig)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncUpstreamPriceManagedModelsUsesTextArray(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	mock.ExpectExec(`ARRAY_AGG\(model_name::text ORDER BY LOWER\(model_name\)\).*ARRAY\[\]::text\[\]`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, syncUpstreamPriceManagedModels(context.Background(), tx))
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
