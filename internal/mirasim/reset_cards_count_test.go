package mirasim

import (
	"context"
	"net/http"
	"testing"
)

// 计数与兑换选卡共用同一判定：只算 active 且未过期的卡，没有到期时间的 active 卡也算。
func TestUsableResetCardsCountsOnlyRedeemableCards(t *testing.T) {
	server := &resetCardsServer{
		cards: `{"cards":[
			{"id":"card-a","status":"active","expires_at":"2099-10-01T00:00:00Z"},
			{"id":"card-b","status":"active"},
			{"id":"card-lapsed","status":"active","expires_at":"2000-01-01T00:00:00Z"},
			{"id":"card-used","status":"used","expires_at":"2099-11-01T00:00:00Z"},
			{"id":"card-revoked","status":"revoked"}
		]}`,
	}
	client, server := newResetCardsTest(t, server)

	count, supported, errCount := client.UsableResetCards(context.Background(), server.host())
	if errCount != nil || !supported || count != 2 {
		t.Fatalf("UsableResetCards() = %d, %v, %v; want 2, true, nil", count, supported, errCount)
	}
	if redeemed := server.redeemPaths(); len(redeemed) != 0 {
		t.Fatalf("counting cards must not redeem any: %v", redeemed)
	}
}

// 查卡随额度一起发起，401 只能作为普通失败返回，不得触发推理通道的票据作废与令牌刷新。
func TestUsableResetCardsLeavesInferenceCredentialsAloneOn401(t *testing.T) {
	server := &resetCardsServer{listStatus: http.StatusUnauthorized, cards: `{"error":"unauthorized"}`}
	client, server := newResetCardsTest(t, server)

	_, _, errCount := client.UsableResetCards(context.Background(), server.host())
	if errCount == nil {
		t.Fatal("a rejected reset-card listing must surface as an error")
	}
	if len(server.paths) != 1 {
		t.Fatalf("a 401 must not be retried: %v", server.paths)
	}
	client.mu.Lock()
	refresh := client.refreshRequired
	client.mu.Unlock()
	if refresh {
		t.Fatal("a reset-card 401 must not mark the access token for refresh")
	}
}

func TestUsableResetCardsReportsAnUnsupportedRelay(t *testing.T) {
	server := &resetCardsServer{listStatus: http.StatusNotFound}
	client, server := newResetCardsTest(t, server)

	count, supported, errCount := client.UsableResetCards(context.Background(), server.host())
	if errCount != nil || supported || count != 0 {
		t.Fatalf("UsableResetCards() = %d, %v, %v; want 0, false, nil", count, supported, errCount)
	}
}
