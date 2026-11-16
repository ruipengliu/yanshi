package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"persona/domain"
	"persona/store/memstore"
)

// foldDigest 是固定账本叠加出的交谈与承诺（即物化行）的摘要。摘要变了说明叠加规则或物化形状变了：
// 递增 domain.FoldVersion，让已写入的物化行不再被按需读取信任，再更新这里的摘要（Persona ADR-0002）。
const foldDigest = "ad9f1ed011187297fc89a6838157051ab920c84b21adcb13579e1441fb8668f2"

const foldDigestVersion = 1

func TestFoldVersionDigest(t *testing.T) {
	s := memstore.New(func() time.Time { return partialNow })
	ctx := context.Background()
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		_, err := partialFixture(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		state := b.State()
		data, err := json.Marshal([]any{state.Exchanges, state.Commitments})
		sum := sha256.Sum256(data)
		digest = hex.EncodeToString(sum[:])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if digest != foldDigest || domain.FoldVersion != foldDigestVersion {
		t.Fatalf("materialized ledger changed (digest %s, FoldVersion %d): bump domain.FoldVersion, then update foldDigest and foldDigestVersion", digest, domain.FoldVersion)
	}
}
