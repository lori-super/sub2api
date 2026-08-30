package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/domain"
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
		WillReturnRows(sqlmock.NewRows([]string{"seen", "expected", "complete"}).AddRow(1, 2, true))
	mock.ExpectRollback()

	_, err = repo.SetModelCatalogStatus(
		context.Background(), "deepseek-new-v1", domain.UpstreamPriceModelStatusManaged,
	)
	require.Error(t, err)
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
