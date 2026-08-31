package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func expectPerRequestSyncPreamble(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(742193847561)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT id FROM channels WHERE id=ANY\(\$1\).*FOR UPDATE`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*channel_model_pricing.*billing_mode='per_request'`).
		WithArgs(sqlmock.AnyArg(), "model-a").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
}

func TestSyncPerRequestPricesAtomicallyUpdatesExistingNativeRows(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	expectPerRequestSyncPreamble(mock)
	mock.ExpectQuery(`(?s)SELECT id,channel_id FROM channel_model_pricing.*jsonb_array_length\(models\)=1.*FOR UPDATE`).
		WithArgs(sqlmock.AnyArg(), "model-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id"}).AddRow(int64(21), int64(2)))
	mock.ExpectQuery(`(?s)SELECT id,min_tokens,max_tokens,sort_order.*channel_pricing_intervals.*FOR UPDATE`).
		WithArgs(int64(21)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "min_tokens", "max_tokens", "sort_order"}).
			AddRow(int64(101), int64(0), int64(256000), 0).
			AddRow(int64(102), int64(256000), int64(512000), 1).
			AddRow(int64(103), int64(512000), nil, 2))
	mock.ExpectQuery(`(?s)SELECT id,enabled,per_request_256k_512k_override::text,.*display_model_prices.*FOR UPDATE`).
		WithArgs("model-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "enabled", "middle", "high"}).
			AddRow(int64(31), true, nil, nil))
	mock.ExpectExec(`(?s)UPDATE channel_model_pricing.*SET per_request_price=\$2::numeric`).
		WithArgs(int64(21), "0.012").WillReturnResult(sqlmock.NewResult(0, 1))
	for _, interval := range []struct {
		id    int64
		price string
	}{{101, "0.012"}, {102, "0.018"}, {103, "0.024"}} {
		mock.ExpectExec(`(?s)UPDATE channel_pricing_intervals.*SET per_request_price=\$2::numeric`).
			WithArgs(interval.id, interval.price, int64(21)).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(`(?s)UPDATE display_model_prices.*SET per_request_lte_256k=\$2::numeric`).
		WithArgs(int64(31), "0.012").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &upstreamPriceMonitorRepository{db: db}
	result, err := repo.SyncPerRequestPrices(context.Background(), []int64{2, 1}, []service.UpstreamPerRequestPriceUpdate{{
		ModelName: "model-a", BasePrice: 0.012, MiddlePrice: 0.018, HighPrice: 0.024,
	}})
	require.NoError(t, err)
	require.Equal(t, &service.UpstreamPerRequestPriceSyncResult{
		Models: 1, ChangedModels: 1, ChangedChannelRows: 1,
	}, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncPerRequestPricesFailsClosedOnDuplicateChannelRows(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	expectPerRequestSyncPreamble(mock)
	mock.ExpectQuery(`(?s)SELECT id,channel_id FROM channel_model_pricing.*jsonb_array_length\(models\)=1.*FOR UPDATE`).
		WithArgs(sqlmock.AnyArg(), "model-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id"}).
			AddRow(int64(21), int64(2)).AddRow(int64(22), int64(1)))
	mock.ExpectRollback()

	repo := &upstreamPriceMonitorRepository{db: db}
	_, err = repo.SyncPerRequestPrices(context.Background(), []int64{1, 2}, []service.UpstreamPerRequestPriceUpdate{{
		ModelName: "model-a", BasePrice: 0.012, MiddlePrice: 0.018, HighPrice: 0.024,
	}})
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSyncPerRequestPricesFailsClosedOnNonNativeIntervals(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	expectPerRequestSyncPreamble(mock)
	mock.ExpectQuery(`(?s)SELECT id,channel_id FROM channel_model_pricing.*jsonb_array_length\(models\)=1.*FOR UPDATE`).
		WithArgs(sqlmock.AnyArg(), "model-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id"}).AddRow(int64(21), int64(2)))
	mock.ExpectQuery(`(?s)SELECT id,min_tokens,max_tokens,sort_order.*channel_pricing_intervals.*FOR UPDATE`).
		WithArgs(int64(21)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "min_tokens", "max_tokens", "sort_order"}).
			AddRow(int64(101), int64(0), int64(256000), 0).
			AddRow(int64(102), int64(256000), nil, 1))
	mock.ExpectRollback()

	repo := &upstreamPriceMonitorRepository{db: db}
	_, err = repo.SyncPerRequestPrices(context.Background(), []int64{1, 2}, []service.UpstreamPerRequestPriceUpdate{{
		ModelName: "model-a", BasePrice: 0.012, MiddlePrice: 0.018, HighPrice: 0.024,
	}})
	require.ErrorIs(t, err, service.ErrUpstreamPriceRunNotApplicable)
	require.NoError(t, mock.ExpectationsWereMet())
}
