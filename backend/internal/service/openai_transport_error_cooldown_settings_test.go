//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGetOpenAITransportErrorCooldownSettingsDefaultsToOfficialBehavior(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})

	settings, err := svc.GetOpenAITransportErrorCooldownSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, 600, settings.CooldownSeconds)
}

func TestSetOpenAITransportErrorCooldownSettingsRoundTrip(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	err := svc.SetOpenAITransportErrorCooldownSettings(context.Background(), &OpenAITransportErrorCooldownSettings{
		Enabled:         false,
		CooldownSeconds: 30,
	})
	require.NoError(t, err)

	settings, err := svc.GetOpenAITransportErrorCooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 30, settings.CooldownSeconds)
}

func TestSetOpenAITransportErrorCooldownSettingsValidatesRange(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})
	for _, seconds := range []int{-1, 0, 7201} {
		err := svc.SetOpenAITransportErrorCooldownSettings(context.Background(), &OpenAITransportErrorCooldownSettings{
			Enabled:         true,
			CooldownSeconds: seconds,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cooldown_seconds must be between 1-7200")
	}
}

func TestGetOpenAITransportErrorCooldownSettingsClampsStoredValue(t *testing.T) {
	repo := newMockSettingRepo()
	raw, err := json.Marshal(OpenAITransportErrorCooldownSettings{Enabled: true, CooldownSeconds: 99999})
	require.NoError(t, err)
	repo.data[SettingKeyOpenAITransportErrorCooldownSettings] = string(raw)
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetOpenAITransportErrorCooldownSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, 7200, settings.CooldownSeconds)
}

func TestOpenAITransportErrorCooldownHotUpdateAppliesWithoutRestart(t *testing.T) {
	accountRepo := &openaiTransportAccountRepoStub{}
	settingRepo := newMockSettingRepo()
	settingService := NewSettingService(settingRepo, &config.Config{})
	svc := &OpenAIGatewayService{
		accountRepo:    accountRepo,
		settingService: settingService,
	}
	account := &Account{ID: 9101, Name: "single-account", Platform: PlatformOpenAI}

	require.NoError(t, settingService.SetOpenAITransportErrorCooldownSettings(context.Background(), &OpenAITransportErrorCooldownSettings{
		Enabled:         false,
		CooldownSeconds: 600,
	}))
	c, _ := newOpenAITransportErrTestContext()
	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, errors.New("dial tcp: connect: connection refused"), false)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, accountRepo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))

	// Update the shared setting while reusing the exact same gateway service.
	require.NoError(t, settingService.SetOpenAITransportErrorCooldownSettings(context.Background(), &OpenAITransportErrorCooldownSettings{
		Enabled:         true,
		CooldownSeconds: 30,
	}))
	before := time.Now()
	c, _ = newOpenAITransportErrTestContext()
	err = svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, errors.New("dial tcp: connect: connection refused"), false)
	require.ErrorAs(t, err, &failoverErr)
	require.Len(t, accountRepo.tempUnschedCalls, 1)
	require.WithinDuration(t, before.Add(30*time.Second), accountRepo.tempUnschedCalls[0].until, 2*time.Second)
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

type transportCooldownClearRepo struct {
	AccountRepository
	accounts []Account
	cleared  []int64
}

func (r *transportCooldownClearRepo) ListActive(context.Context) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func (r *transportCooldownClearRepo) ClearTempUnschedulable(_ context.Context, accountID int64) error {
	r.cleared = append(r.cleared, accountID)
	return nil
}

func TestClearOpenAITransportErrorCooldownsRestoresOnlyMatchingAccounts(t *testing.T) {
	now := time.Now()
	transportAccount := Account{
		ID: 9201, Name: "transport", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		TempUnschedulableUntil:  transportTimePtr(now.Add(10 * time.Minute)),
		TempUnschedulableReason: openAITransportErrorTempUnschedReasonPrefix + "connection refused",
	}
	otherAccount := Account{
		ID: 9202, Name: "other", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		TempUnschedulableUntil:  transportTimePtr(now.Add(10 * time.Minute)),
		TempUnschedulableReason: "token refresh retry exhausted: timeout",
	}
	repo := &transportCooldownClearRepo{accounts: []Account{transportAccount, otherAccount}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	svc.BlockAccountScheduling(&transportAccount, *transportAccount.TempUnschedulableUntil, "transport_error")
	svc.BlockAccountScheduling(&otherAccount, *otherAccount.TempUnschedulableUntil, "token_refresh")

	cleared, err := svc.ClearOpenAITransportErrorCooldowns(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, cleared)
	require.Equal(t, []int64{transportAccount.ID}, repo.cleared)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(&transportAccount))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(&otherAccount))
}

func transportTimePtr(value time.Time) *time.Time {
	return &value
}
