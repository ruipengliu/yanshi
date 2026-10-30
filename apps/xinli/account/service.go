package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"xinli/task"
)

// IdentityProvider 只证明身份；owner 由应用生成，不能由客户端指定。
type IdentityProvider interface {
	Verify(context.Context, string, string) (string, error)
}

type TestIdentity struct{ BusinessLine, Subject, Code string }
type TestProvider struct{ Identities []TestIdentity }

func (p TestProvider) Verify(ctx context.Context, line, code string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if code == "" {
		return "", ErrUnauthorized
	}
	got := sha256.Sum256([]byte(code))
	for _, v := range p.Identities {
		expected := sha256.Sum256([]byte(v.Code))
		if v.BusinessLine == line && v.Code != "" && v.Subject != "" && subtle.ConstantTimeCompare(got[:], expected[:]) == 1 {
			return v.Subject, nil
		}
	}
	return "", ErrUnauthorized
}

type Service struct {
	Store      Store
	Provider   IdentityProvider
	Now        func() time.Time
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	// 仅旧 dev 夹具可以使用尚未注册的 owner；生产必须保持 false。
	AllowUnregistered bool
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC().Truncate(time.Microsecond)
	}
	return time.Now().UTC().Truncate(time.Microsecond)
}
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
func Hash(token string) string { h := sha256.Sum256([]byte(token)); return hex.EncodeToString(h[:]) }

type Credentials struct {
	Owner            task.Owner `json:"owner"`
	DeviceID         string     `json:"device_id"`
	AccessToken      string     `json:"access_token"`
	RefreshToken     string     `json:"refresh_token"`
	AccessExpiresAt  time.Time  `json:"access_expires_at"`
	RefreshExpiresAt time.Time  `json:"refresh_expires_at"`
}

func (s *Service) newSession(device string) (Session, Credentials, error) {
	access, err := randomToken()
	if err != nil {
		return Session{}, Credentials{}, err
	}
	refresh, err := randomToken()
	if err != nil {
		return Session{}, Credentials{}, err
	}
	id, err := randomToken()
	if err != nil {
		return Session{}, Credentials{}, err
	}
	now := s.now()
	a, r := s.AccessTTL, s.RefreshTTL
	if a <= 0 {
		a = 15 * time.Minute
	}
	if r <= 0 {
		r = 30 * 24 * time.Hour
	}
	if r < a {
		return Session{}, Credentials{}, ErrInvalid
	}
	session := Session{ID: id, DeviceID: device, AccessHash: Hash(access), RefreshHash: Hash(refresh), AccessExpiresAt: now.Add(a), RefreshExpiresAt: now.Add(r), CreatedAt: now}
	return session, Credentials{DeviceID: device, AccessToken: access, RefreshToken: refresh, AccessExpiresAt: session.AccessExpiresAt, RefreshExpiresAt: session.RefreshExpiresAt}, nil
}

func (s *Service) Login(ctx context.Context, line, proof, device, platform string) (Credentials, error) {
	if s.Provider == nil || line == "" || device == "" || len(device) > 128 || platform == "" || len(platform) > 64 {
		return Credentials{}, ErrUnauthorized
	}
	subject, err := s.Provider.Verify(ctx, line, proof)
	if err != nil {
		return Credentials{}, ErrUnauthorized
	}
	id, err := randomToken()
	if err != nil {
		return Credentials{}, err
	}
	// 哈希编码包含业务线；同一身份在另一业务线不能取得此账号。
	identity, _ := json.Marshal([]string{line, subject})
	now := s.now()
	a, err := s.Store.Register(ctx, Hash(string(identity)), Account{Owner: task.Owner{BusinessLine: line, EndUser: "eu_" + id}, Status: Active, TimeZone: "Asia/Shanghai", CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return Credentials{}, err
	}
	session, out, err := s.newSession(device)
	if err != nil {
		return Credentials{}, err
	}
	out.Owner = a.Owner
	err = s.WithActive(ctx, a.Owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, a.Owner, func(r *Record) error {
			// 同一安装实例重新登录轮换其所有凭据；其他安装实例不受影响。
			revoke(r, device, s.now())
			r.Devices[device] = Device{ID: device, Platform: platform, BoundAt: s.now()}
			r.Sessions[session.ID] = session
			return nil
		})
	})
	if err != nil {
		return Credentials{}, err
	}
	return out, nil
}

func (s *Service) WithActive(ctx context.Context, owner task.Owner, fn func(context.Context) error) error {
	return s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		r, err := s.Store.ReadAccount(ctx, owner)
		if err == ErrNotFound && s.AllowUnregistered {
			return fn(ctx)
		}
		if err != nil {
			return err
		}
		if r.Account.Status != Active {
			return ErrInactive
		}
		return fn(ctx)
	})
}

func validSession(r Record, hash string, refresh bool, now time.Time) (Session, bool) {
	if r.Account.Status != Active {
		return Session{}, false
	}
	for _, ss := range r.Sessions {
		d, ok := r.Devices[ss.DeviceID]
		if !ok || !d.RevokedAt.IsZero() || !ss.RevokedAt.IsZero() {
			continue
		}
		if refresh && ss.RefreshHash == hash && now.Before(ss.RefreshExpiresAt) || !refresh && ss.AccessHash == hash && now.Before(ss.AccessExpiresAt) {
			return ss, true
		}
	}
	return Session{}, false
}

// WithToken 把鉴权与一次应用操作放进同一个 owner gate；撤销返回后旧请求不能再进入。
func (s *Service) WithToken(ctx context.Context, token string, fn func(context.Context, task.Owner, string) error) error {
	if len(token) != 43 {
		return ErrUnauthorized
	}
	hash := Hash(token)
	owner, err := s.Store.TokenOwner(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrUnauthorized
		}
		return err
	}
	return s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		r, err := s.Store.ReadAccount(ctx, owner)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrUnauthorized
			}
			return err
		}
		ss, ok := validSession(r, hash, false, s.now())
		if !ok {
			return ErrUnauthorized
		}
		return fn(ctx, owner, ss.DeviceID)
	})
}

func (s *Service) Refresh(ctx context.Context, token string) (Credentials, error) {
	if len(token) != 43 {
		return Credentials{}, ErrUnauthorized
	}
	hash := Hash(token)
	owner, err := s.Store.TokenOwner(ctx, hash)
	if err != nil {
		return Credentials{}, ErrUnauthorized
	}
	var out Credentials
	err = s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error {
			old, ok := validSession(*r, hash, true, s.now())
			if !ok {
				return ErrUnauthorized
			}
			next, creds, err := s.newSession(old.DeviceID)
			if err != nil {
				return err
			}
			// 删除旧哈希使 refresh 单次使用；并发刷新只有一个能成功。
			delete(r.Sessions, old.ID)
			r.Sessions[next.ID] = next
			out = creds
			out.Owner = owner
			return nil
		})
	})
	if err != nil {
		return Credentials{}, err
	}
	return out, nil
}
func revoke(r *Record, device string, now time.Time) {
	for id, ss := range r.Sessions {
		if device == "" || ss.DeviceID == device {
			delete(r.Sessions, id)
		}
	}
	for id, d := range r.Devices {
		if device == "" || id == device {
			d.RevokedAt = now
			r.Devices[id] = d
		}
	}
}
func (s *Service) Logout(ctx context.Context, owner task.Owner, device string) error {
	return s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error { revoke(r, device, s.now()); return nil })
	})
}
func (s *Service) Disable(ctx context.Context, owner task.Owner) error {
	return s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error {
			if r.Account.Status != Active && r.Account.Status != Disabled {
				return ErrInactive
			}
			r.Account.Status = Disabled
			r.Account.UpdatedAt = s.now()
			revoke(r, "", s.now())
			return nil
		})
	})
}
func (s *Service) Profile(ctx context.Context, owner task.Owner, zone string, prefs json.RawMessage) error {
	if _, err := time.LoadLocation(zone); err != nil || len(prefs) > 16384 || len(prefs) > 0 && !json.Valid(prefs) {
		return ErrInvalid
	}
	return s.WithActive(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error {
			r.Account.TimeZone = zone
			r.Account.Preferences = append(json.RawMessage(nil), prefs...)
			r.Account.UpdatedAt = s.now()
			return nil
		})
	})
}

const TierOperation = "account_tier"
const DeleteOperation = "account_delete"

func IsTask(t task.Task) bool { return t.Operation == TierOperation || t.Operation == DeleteOperation }
func accountTask(owner task.Owner, op string, version uint64, now time.Time) task.Task {
	return task.New(task.Source{Owner: owner, Kind: "account", ID: op, Version: version}, task.Spec{Operation: op, Target: op}, now)
}

// SetEntitlement 仅供受信权益系统调用；HTTP 登录面没有设置档位的路由。
func (s *Service) SetEntitlement(ctx context.Context, owner task.Owner, tier string) error {
	if len(tier) > 128 {
		return ErrInvalid
	}
	return s.WithActive(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error {
			if r.Account.EntitlementVersion >= 1<<63-1 {
				return ErrInvalid
			}
			r.Account.Entitlement = tier
			r.Account.EntitlementVersion++
			r.Account.UpdatedAt = s.now()
			r.Enqueue = append(r.Enqueue, accountTask(owner, TierOperation, r.Account.EntitlementVersion, s.now()))
			return nil
		})
	})
}
func (s *Service) BeginDeletion(ctx context.Context, owner task.Owner) (Deletion, error) {
	return s.beginDeletion(ctx, owner, "")
}
func (s *Service) beginDeletion(ctx context.Context, owner task.Owner, receiptHash string) (Deletion, error) {
	var out Deletion
	err := s.Store.WithOwner(ctx, owner, func(ctx context.Context) error {
		return s.Store.UpdateAccount(ctx, owner, func(r *Record) error {
			if r.Account.Status == Deleting || r.Account.Status == Deleted {
				if receiptHash != "" {
					r.Deletion.ReceiptHash = receiptHash
				}
				out = r.Deletion
				return nil
			}
			r.Account.Status = Deleting
			r.Account.UpdatedAt = s.now()
			revoke(r, "", s.now())
			r.Deletion = Deletion{ReceiptHash: receiptHash, Stage: "persona_barrier", StartedAt: s.now()}
			out = r.Deletion
			r.Enqueue = append(r.Enqueue, accountTask(owner, DeleteOperation, 1, s.now()))
			return nil
		})
	})
	return out, err
}
func (s *Service) Deletion(ctx context.Context, owner task.Owner) (Deletion, error) {
	r, err := s.Store.ReadAccount(ctx, owner)
	if err != nil {
		return Deletion{}, err
	}
	if r.Deletion.Stage == "" {
		return Deletion{}, ErrNotFound
	}
	return r.Deletion, nil
}

// 注销撤销登录态后，单独的只读凭据仍能查询进度，不能用于任何账号操作。
func (s *Service) RequestDeletion(ctx context.Context, owner task.Owner) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = s.beginDeletion(ctx, owner, Hash(token))
	if err != nil {
		return "", err
	}
	return token, nil
}
func (s *Service) DeletionWithToken(ctx context.Context, token string) (Deletion, error) {
	if len(token) != 43 {
		return Deletion{}, ErrUnauthorized
	}
	owner, err := s.Store.DeletionOwner(ctx, Hash(token))
	if err != nil {
		return Deletion{}, ErrUnauthorized
	}
	out, err := s.Deletion(ctx, owner)
	out.ReceiptHash = ""
	return out, err
}
