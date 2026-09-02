package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestSyncTokenPricesAtomicallyUpdatesChannelBaseIntervalAndCreatesDisplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo, ok := NewUpstreamPriceMonitorRepository(db).(*upstreamPriceMonitorRepository)
	require.True(t, ok)
	officialInput, input, output, cacheWrite, cacheRead := 17.5, 0.129, 0.37908, 0.00084, 0.01284
	updates := []service.UpstreamTokenPriceUpdate{{
		ModelName: "qwen3.8-flash", Provider: "qwen", OfficialInput: &officialInput,
		InputPerMillion: &input, OutputPerMillion: &output,
		CacheWritePerMillion: &cacheWrite, CacheReadPerMillion: &cacheRead,
	}}

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM channels WHERE id=ANY\(\$1\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(8)))
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*channel_model_pricing cmp.*jsonb_array_length\(models\)<>1`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"qwen3.8-flash"})).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT id,channel_id,models->>0.*channel_model_pricing.*LOWER\(models->>0\)=ANY\(\$2\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"qwen3.8-flash"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "model"}).
			AddRow(int64(71), int64(8), "qwen3.8-flash"))
	mock.ExpectExec(`(?s)UPDATE channel_model_pricing SET.*input_price=\$2::numeric.*billing_mode='token'`).
		WithArgs(int64(71), "0.000000129", "0.00000037908", "0.00000000084", "0.00000001284").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE channel_pricing_intervals SET.*input_price=\$2::numeric.*min_tokens=0`).
		WithArgs(int64(71), "0.000000129", "0.00000037908", "0.00000000084", "0.00000001284").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT id FROM display_model_prices.*platform='openai'.*LOWER\(model_name\)=LOWER\(\$1\).*FOR UPDATE`).
		WithArgs("qwen3.8-flash").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`(?s)INSERT INTO display_model_prices.*FROM display_pricing_providers.*RETURNING id`).
		WithArgs("qwen3.8-flash", "qwen", officialInput, nil, nil, nil, input, output, cacheWrite, cacheRead,
			service.DisplayOfficialPriceX5M5X, service.DisplayUpstreamPriceSourceURL, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(901)))
	mock.ExpectCommit()

	result, err := repo.SyncTokenPrices(context.Background(), []int64{8}, updates)
	require.NoError(t, err)
	require.Equal(t, 1, result.Models)
	require.Equal(t, 1, result.ChangedModels)
	require.Equal(t, 1, result.ChangedChannelRows)
	require.Equal(t, 1, result.ChangedIntervalRows)
	require.Equal(t, 1, result.ChangedDisplayRows)
	require.Equal(t, 1, result.CreatedDisplayRows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncTokenPricesRollsBackWhenAnyConfiguredModelHasNoChannelRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo, ok := NewUpstreamPriceMonitorRepository(db).(*upstreamPriceMonitorRepository)
	require.True(t, ok)
	input, output := 0.1, 0.2
	updates := []service.UpstreamTokenPriceUpdate{
		{ModelName: "deepseek-v4-flash-0731", Provider: "deepseek", InputPerMillion: &input, OutputPerMillion: &output},
		{ModelName: "qwen3.8-flash", Provider: "qwen", InputPerMillion: &input, OutputPerMillion: &output},
	}

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM channels WHERE id=ANY\(\$1\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(8)))
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*channel_model_pricing cmp.*jsonb_array_length\(models\)<>1`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"deepseek-v4-flash-0731", "qwen3.8-flash"})).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT id,channel_id,models->>0.*channel_model_pricing.*LOWER\(models->>0\)=ANY\(\$2\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"deepseek-v4-flash-0731", "qwen3.8-flash"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "model"}).
			AddRow(int64(70), int64(8), "deepseek-v4-flash-0731"))
	mock.ExpectRollback()

	_, err = repo.SyncTokenPrices(context.Background(), []int64{8}, updates)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.ErrorContains(t, err, "qwen3.8-flash")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncTokenPricesRollsBackOnDuplicateSingleModelRowsInOneChannel(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo, ok := NewUpstreamPriceMonitorRepository(db).(*upstreamPriceMonitorRepository)
	require.True(t, ok)
	input, output := 0.1, 0.2
	updates := []service.UpstreamTokenPriceUpdate{{
		ModelName: "deepseek-v4-flash-0731", Provider: "deepseek",
		InputPerMillion: &input, OutputPerMillion: &output,
	}}

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id FROM channels WHERE id=ANY\(\$1\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8})).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(8)))
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*channel_model_pricing cmp.*jsonb_array_length\(models\)<>1`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"deepseek-v4-flash-0731"})).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT id,channel_id,models->>0.*channel_model_pricing.*LOWER\(models->>0\)=ANY\(\$2\).*FOR UPDATE`).
		WithArgs(pq.Array([]int64{8}), pq.Array([]string{"deepseek-v4-flash-0731"})).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "model"}).
			AddRow(int64(70), int64(8), "deepseek-v4-flash-0731").
			AddRow(int64(71), int64(8), "DeepSeek-v4-flash-0731"))
	mock.ExpectRollback()

	_, err = repo.SyncTokenPrices(context.Background(), []int64{8}, updates)
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.ErrorContains(t, err, "duplicate token pricing rows")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncTokenDisplayPriceUpdatesExistingExactOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	officialInput, input, output, cacheRead := 1.6, 0.096, 0.282, 0.012
	update := service.UpstreamTokenPriceUpdate{
		ModelName: "deepseek-v4-flash-0731", Provider: "deepseek", OfficialInput: &officialInput,
		InputPerMillion: &input, OutputPerMillion: &output, CacheReadPerMillion: &cacheRead,
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT id FROM display_model_prices.*platform='openai'.*LOWER\(model_name\)=LOWER\(\$1\).*FOR UPDATE`).
		WithArgs("deepseek-v4-flash-0731").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(501)))
	mock.ExpectExec(`(?s)UPDATE display_model_prices SET.*official_input_per_million=.*display_input_per_million_override=.*official_price_source`).
		WithArgs(int64(501), officialInput, nil, nil, nil, input, output, nil, cacheRead,
			service.DisplayOfficialPriceX5M5X, service.DisplayUpstreamPriceSourceURL, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	changed, created, err := syncTokenDisplayPrice(context.Background(), tx, update.ModelName, update)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, created)
	require.NoError(t, tx.Commit())
	require.NoError(t, mock.ExpectationsWereMet())
}
