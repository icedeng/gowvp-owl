package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/web"
)

type UserAPI struct {
	conf          *conf.Bootstrap
	secret        *Secret
	credentialsMu *sync.RWMutex
	sockets       *adminSockets
}

type Secret struct {
	privateKey    *rsa.PrivateKey
	publicKey     *rsa.PublicKey
	expiredAt     time.Time
	m             sync.RWMutex
	previousKey   *rsa.PrivateKey
	previousUntil time.Time
}

const loginKeyGrace = 5 * time.Minute

func loginKeyID(key *rsa.PublicKey) string {
	encoded, _ := x509.MarshalPKIXPublicKey(key)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (s *Secret) GetOrCreatePublicKey() (*rsa.PublicKey, error) {
	s.m.RLock()
	if s.publicKey != nil && time.Now().Before(s.expiredAt) {
		key := s.publicKey
		s.m.RUnlock()
		return key, nil
	}
	s.m.RUnlock()

	s.m.Lock()
	defer s.m.Unlock()
	// 等待写锁期间可能已有请求完成密钥初始化，避免并发请求互相作废公钥。
	if s.publicKey != nil && time.Now().Before(s.expiredAt) {
		return s.publicKey, nil
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	if s.privateKey != nil && time.Now().Before(s.expiredAt.Add(loginKeyGrace)) {
		s.previousKey, s.previousUntil = s.privateKey, s.expiredAt.Add(loginKeyGrace)
	} else {
		s.previousKey, s.previousUntil = nil, time.Time{}
	}
	s.privateKey = privateKey
	s.publicKey = &privateKey.PublicKey
	s.expiredAt = time.Now().Add(1 * time.Hour)
	return s.publicKey, nil
}

func (s *Secret) MarshalPKIXPublicKey(key *rsa.PublicKey) []byte {
	publicKeyBytes, _ := x509.MarshalPKIXPublicKey(key)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyBytes,
	})
}

func (s *Secret) Decrypt(ciphertext string, keyIDs ...string) ([]byte, error) {
	s.m.RLock()
	pri := s.privateKey
	until := s.expiredAt.Add(loginKeyGrace)
	previous, previousUntil := s.previousKey, s.previousUntil
	s.m.RUnlock()
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}
	keyID := ""
	if len(keyIDs) > 0 {
		keyID = keyIDs[0]
	}
	for _, candidate := range []struct {
		key   *rsa.PrivateKey
		until time.Time
	}{{pri, until}, {previous, previousUntil}} {
		if candidate.key == nil || !time.Now().Before(candidate.until) || (keyID != "" && keyID != loginKeyID(&candidate.key.PublicKey)) {
			continue
		}
		if plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, candidate.key, data, nil); err == nil {
			return plaintext, nil
		}
	}
	return nil, fmt.Errorf("登录密钥已失效或密文无效，请刷新后重试")
}

func NewUserAPI(conf *conf.Bootstrap) UserAPI {
	return UserAPI{
		conf:          conf,
		secret:        &Secret{},
		credentialsMu: new(sync.RWMutex),
		sockets:       &adminSockets{},
	}
}

func RegisterUser(r gin.IRouter, api UserAPI, mid ...gin.HandlerFunc) {
	guard := newLoginGuard()
	r.POST("/login", guard.middleware(false), web.WrapH(api.login))
	r.GET("/login/key", guard.middleware(true), web.WrapH(api.getPublicKey))

	group := r.Group("/users", mid...)
	group.PUT("", web.WrapH(api.updateCredentials))
}

// 登录请求结构体
type loginInput struct {
	// Username string `json:"username" binding:"required"`
	// Password string `json:"password" binding:"required"`
	Data  string `json:"data" binding:"required"`
	KeyID string `json:"key_id"`
}

// 登录响应结构体
type loginOutput struct {
	Token string `json:"token"`
	User  string `json:"user"`
}

// 登录接口
func (api UserAPI) login(c *gin.Context, in *loginInput) (*loginOutput, error) {
	body, err := api.secret.Decrypt(in.Data, in.KeyID)
	if err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &credentials); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}

	// 凭据必须由安全配置初始化，不再回退到固定默认密码。
	api.credentialsMu.RLock()
	defer api.credentialsMu.RUnlock()
	if api.conf.Server.Username == "" || api.conf.Server.PasswordHash == "" {
		return nil, reason.ErrServer.WithMsg("管理员凭据未配置")
	}
	if credentials.Username != api.conf.Server.Username || !conf.VerifyAdminPassword(api.conf.Server.PasswordHash, credentials.Password) {
		if c != nil {
			c.Set("login_credentials_result", false)
		}
		return nil, reason.ErrNameOrPasswd
	}
	if c != nil {
		c.Set("login_credentials_result", true)
	}

	data := web.NewClaimsData().SetUsername(credentials.Username)
	data[sessionVersionClaim] = api.conf.AdminSessionVersion

	token, err := web.NewToken(data, api.conf.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(3*24*time.Hour)))
	if err != nil {
		return nil, reason.ErrServer.WithMsg("生成token失败: " + err.Error())
	}

	return &loginOutput{
		Token: token,
		User:  credentials.Username,
	}, nil
}

// 修改凭据请求结构体（加密传输）
type updateCredentialsInput struct {
	Data  string `json:"data" binding:"required"`
	KeyID string `json:"key_id"`
}

// 修改凭据接口，需旧密码校验通过才允许修改
func (api UserAPI) updateCredentials(_ *gin.Context, in *updateCredentialsInput) (gin.H, error) {
	body, err := api.secret.Decrypt(in.Data, in.KeyID)
	if err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	var credentials struct {
		OldPassword string `json:"old_password"`
		Username    string `json:"username"`
		Password    string `json:"password"`
	}
	if err := json.Unmarshal(body, &credentials); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}

	if credentials.Username == "" || credentials.Password == "" {
		return nil, reason.ErrServer.WithMsg("账号和密码不能为空")
	}

	// 校验旧密码
	api.credentialsMu.Lock()
	defer api.credentialsMu.Unlock()
	if !conf.VerifyAdminPassword(api.conf.Server.PasswordHash, credentials.OldPassword) {
		return nil, reason.ErrServer.WithMsg("旧密码错误")
	}

	hash, version, err := conf.SaveAdminCredentials(api.conf, credentials.Username, credentials.Password)
	if err != nil {
		return nil, reason.ErrServer.WithMsg("保存配置失败: " + err.Error())
	}
	api.conf.Server.Username = credentials.Username
	api.conf.Server.Password = ""
	api.conf.Server.PasswordHash = hash
	api.conf.AdminSessionVersion = version
	api.sockets.reset()

	return gin.H{"msg": "凭据更新成功"}, nil
}

func (api UserAPI) getPublicKey(c *gin.Context, _ *struct{}) (gin.H, error) {
	if c != nil {
		c.Header("Cache-Control", "no-store")
	}
	publicKey, err := api.secret.GetOrCreatePublicKey()
	if err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	oaepSeed := make([]byte, sha256.Size)
	if _, err := rand.Read(oaepSeed); err != nil {
		return nil, reason.ErrServer.WithMsg(err.Error())
	}
	result := api.secret.MarshalPKIXPublicKey(publicKey)
	return gin.H{
		"key":       base64.StdEncoding.EncodeToString(result),
		"key_id":    loginKeyID(publicKey),
		"oaep_seed": base64.StdEncoding.EncodeToString(oaepSeed),
	}, nil
}
