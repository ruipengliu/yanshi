package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"yanshi/internal/auth"
	"yanshi/sdk/nodesdk"
)

// tokenFlags 为 chat 与 node 提供令牌：-token 使用固定令牌；-key 用业务线私钥自行签发，
// 即在开发时扮演业务线服务端（docs/design/auth.md §5）。都不给时不带令牌（serve -auth none）。
type tokenFlags struct {
	token, key, audience *string
	ttl                  *time.Duration
}

func addTokenFlags(fs *flag.FlagSet) *tokenFlags {
	return &tokenFlags{
		token:    fs.String("token", os.Getenv("YANSHI_TOKEN"), "访问令牌（也可用环境变量 YANSHI_TOKEN）"),
		key:      fs.String("key", os.Getenv("YANSHI_KEY"), "业务线私钥文件（yanshi keygen 生成）；给出时自行签发令牌，业务线取自密钥"),
		audience: fs.String("audience", "yanshi", "令牌的 aud，须与 serve -auth-audience 一致"),
		ttl:      fs.Duration("token-ttl", time.Hour, "自行签发的令牌有效期"),
	}
}

// source 返回令牌来源与生效的业务线（-key 时取自密钥，否则为 businessLine）。
func (f *tokenFlags) source(businessLine, endUser string) (nodesdk.TokenSource, string, error) {
	switch {
	case *f.key != "":
		k, err := auth.LoadSigningKey(*f.key)
		if err != nil {
			return nil, "", err
		}
		return signer(k, *f.audience, endUser, *f.ttl), k.BusinessLine, nil
	case *f.token != "":
		t := *f.token
		return func(context.Context) (string, error) { return t, nil }, businessLine, nil
	}
	return nil, businessLine, nil
}

// signer 缓存签发的令牌，在到期前一分钟重新签发。
func signer(k *auth.SigningKey, audience, endUser string, ttl time.Duration) nodesdk.TokenSource {
	var mu sync.Mutex
	var token string
	var exp time.Time
	return func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if now := time.Now(); token == "" || now.After(exp.Add(-time.Minute)) {
			t, err := k.Sign(audience, endUser, ttl, now)
			if err != nil {
				return "", err
			}
			token, exp = t, now.Add(ttl)
		}
		return token, nil
	}
}

// keygen 生成开发用的业务线密钥对：私钥文件给扮演业务线的工具使用，公钥配置登记到 serve。
func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	bl := fs.String("business-line", "demo", "业务线名")
	keyOut := fs.String("key-out", "", "私钥输出路径，默认 <业务线>.key")
	dir := fs.String("businesslines", "businesslines", "业务线配置目录（serve -businesslines）")
	_ = fs.Parse(args)
	if *keyOut == "" {
		*keyOut = *bl + ".key"
	}
	k, err := auth.GenerateDevKey(*bl, fmt.Sprintf("%s-%s", *bl, time.Now().Format("20060102-150405")))
	if err != nil {
		return err
	}
	priv, err := k.MarshalPEM()
	if err != nil {
		return err
	}
	pub, err := k.Public()
	if err != nil {
		return err
	}
	conf, err := yaml.Marshal(pub)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(*dir, *bl+".yaml")
	for _, p := range []string{*keyOut, cfgPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite", p)
		}
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*keyOut, priv, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, conf, 0o644); err != nil {
		return err
	}
	fmt.Printf("私钥 %s（勿提交、勿部署到 yanshi）\n业务线配置 %s（kid %s）\n", *keyOut, cfgPath, k.KID)
	return nil
}

// tokenCmd 用业务线私钥签发一个令牌并打印。
func tokenCmd(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	key := fs.String("key", os.Getenv("YANSHI_KEY"), "业务线私钥文件")
	user := fs.String("user", "", "终端用户；为空时签发服务令牌")
	audience := fs.String("audience", "yanshi", "令牌的 aud")
	ttl := fs.Duration("ttl", time.Hour, "有效期")
	_ = fs.Parse(args)
	if *key == "" {
		return errors.New("-key is required")
	}
	k, err := auth.LoadSigningKey(*key)
	if err != nil {
		return err
	}
	t, err := k.Sign(*audience, *user, *ttl, time.Now())
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}
