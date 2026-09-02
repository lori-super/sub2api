package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSelectActiveProbeModelsUsesPersistentLeastRecentRotation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(`(?s)WITH requested AS.*MAX\(created_at\).*source='active_probe'.*billing_mode='token'.*NULLS FIRST.*LIMIT \$2`).
		WithArgs(sqlmock.AnyArg(), 3).
		WillReturnRows(sqlmock.NewRows([]string{"model_name"}).
			AddRow("never-probed").
			AddRow("least-recent").
			AddRow("next-least-recent"))

	repo := &upstreamPriceMonitorRepository{db: db}
	selected, err := repo.SelectActiveProbeModels(context.Background(), []string{
		"fresh", "never-probed", "least-recent", "next-least-recent",
	}, 3)
	require.NoError(t, err)
	require.Equal(t, []string{"never-probed", "least-recent", "next-least-recent"}, selected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSelectActiveProbeModelsDeduplicatesScopeAndCapsLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(`(?s)WITH requested AS.*LIMIT \$2`).
		WithArgs(sqlmock.AnyArg(), 2).
		WillReturnRows(sqlmock.NewRows([]string{"model_name"}).AddRow("Model-A").AddRow("Model-B"))

	repo := &upstreamPriceMonitorRepository{db: db}
	selected, err := repo.SelectActiveProbeModels(context.Background(), []string{
		" Model-A ", "model-a", "Model-B",
	}, 9)
	require.NoError(t, err)
	require.Equal(t, []string{"Model-A", "Model-B"}, selected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSelectActiveProbeModelsRejectsInvalidLimit(t *testing.T) {
	repo := &upstreamPriceMonitorRepository{}
	_, err := repo.SelectActiveProbeModels(context.Background(), []string{"model-a"}, 0)
	require.Error(t, err)
}
