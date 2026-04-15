# 优化 F：安全体系设计（Security Architecture）

> **定位**：P0 级——管控平台能在任意节点执行 Shell 命令 = 最高权限操作，安全是必答题  
> **范围**：Agent 认证鉴权、命令执行安全、API 安全、数据安全、审计日志  
> **核心价值**：从"能用"到"安全可控"  
> **面试价值**：⭐⭐⭐⭐⭐ — 大厂面试对管控/运维平台必问安全设计

---

## 一、威胁模型分析：为什么管控平台的安全是最高优先级

### 1.1 攻击面全景

管控平台与普通 Web 应用的本质区别：**它能在远程节点上执行任意 Shell 命令**。

```
普通 Web 应用的最大危害：数据泄露
管控平台的最大危害：整个集群的 Root 权限被夺取
```

攻击面分析：

| 攻击面 | 威胁描述 | 严重程度 | 典型攻击路径 |
|--------|---------|---------|------------|
| **Agent 伪造** | 攻击者部署伪造 Agent，接管节点控制权 | 🔴 Critical | 伪造 Agent → 注册到 Server → 拉取其他节点的 Action |
| **命令注入** | 通过 Job 参数注入恶意 Shell 命令 | 🔴 Critical | `rm -rf /` 注入到 Action 的 command 字段 |
| **中间人攻击** | 窃听/篡改 gRPC 通信内容 | 🟠 High | 劫持 Agent → Server 的网络流量 |
| **越权操作** | 低权限用户创建高危 Job（如格式化磁盘） | 🟠 High | 普通运维误操作 / 恶意操作 |
| **数据泄露** | DB 连接串、Redis 密码等敏感信息明文存储 | 🟡 Medium | 配置文件泄露 → 直连数据库 |
| **API 滥用** | 未授权访问 / 重放攻击 / 暴力请求 | 🟡 Medium | 绕过认证直接调用 CreateJob |
| **审计缺失** | 无法追溯"谁在什么时间执行了什么操作" | 🟡 Medium | 出了事故找不到责任人 |

### 1.2 与业界事故的对照

| 事故 | 根因 | 本项目对应威胁 |
|------|------|--------------|
| SaltStack CVE-2020-11651 | 认证绕过 + 命令注入 | Agent 伪造 + 命令注入 |
| Ansible Tower 越权漏洞 | RBAC 设计缺陷 | 越权操作 |
| Jenkins Script Console RCE | 管理接口未鉴权 | API 滥用 |

**核心结论**：管控平台的安全设计不是"锦上添花"，而是"没有就不能上线"。

---

## 二、Agent 认证与鉴权（核心）

### 2.1 方案选型

| 方案 | 安全级别 | 实现复杂度 | 适用场景 |
|------|---------|-----------|---------|
| **mTLS 双向证书认证** | ⭐⭐⭐⭐⭐ | 中 | 生产环境首选 |
| JWT Token | ⭐⭐⭐⭐ | 低 | Agent 数量少、网络可信 |
| Pre-Shared Key | ⭐⭐⭐ | 低 | 开发/测试环境 |
| IP 白名单 | ⭐⭐ | 极低 | 辅助手段，不能单独使用 |

**选择 mTLS + JWT 组合方案**：
- **mTLS** 解决传输层安全（加密 + Agent 身份认证）
- **JWT** 解决应用层鉴权（Agent 权限范围、过期机制）

### 2.2 mTLS 双向认证架构

```
┌─────────────────────────────────────────────────────────────────────┐
│                         证书体系                                     │
│                                                                      │
│    ┌──────────┐                                                      │
│    │  CA Root  │  ← 内部 CA（cfssl 自签）                             │
│    │  证书     │                                                      │
│    └────┬─────┘                                                      │
│         │ 签发                                                       │
│    ┌────┴────────────────────┐                                       │
│    │                         │                                       │
│    ▼                         ▼                                       │
│  ┌────────────┐        ┌────────────┐                                │
│  │ Server 证书 │        │ Agent 证书  │  ← 每个 Agent 独立证书          │
│  │ server.pem │        │ agent-{id} │    CN = Agent UUID              │
│  │ server.key │        │ .pem/.key  │                                │
│  └────────────┘        └────────────┘                                │
│                                                                      │
│  Server 验证 Agent 证书 → 确认 Agent 身份                              │
│  Agent 验证 Server 证书 → 防止连到伪造 Server                          │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.3 gRPC mTLS 实现

```go
// internal/server/grpc/tls_config.go

package grpc

import (
    "crypto/tls"
    "crypto/x509"
    "os"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
)

// NewServerTLSConfig 创建 Server 端 mTLS 配置
func NewServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
    // 加载 Server 证书
    cert, err := tls.LoadX509KeyPair(certFile, keyFile)
    if err != nil {
        return nil, fmt.Errorf("load server cert: %w", err)
    }

    // 加载 CA 证书（用于验证 Agent 证书）
    caCert, err := os.ReadFile(caFile)
    if err != nil {
        return nil, fmt.Errorf("read CA cert: %w", err)
    }
    caPool := x509.NewCertPool()
    caPool.AppendCertsFromPEM(caCert)

    return &tls.Config{
        Certificates: []tls.Certificate{cert},
        ClientAuth:   tls.RequireAndVerifyClientCert, // 强制验证客户端证书
        ClientCAs:    caPool,
        MinVersion:   tls.VersionTLS13, // 最低 TLS 1.3
    }, nil
}

// NewSecureGRPCServer 创建带 mTLS 的 gRPC Server
func NewSecureGRPCServer(certFile, keyFile, caFile string) (*grpc.Server, error) {
    tlsConfig, err := NewServerTLSConfig(certFile, keyFile, caFile)
    if err != nil {
        return nil, err
    }

    creds := credentials.NewTLS(tlsConfig)
    server := grpc.NewServer(
        grpc.Creds(creds),
        grpc.ChainUnaryInterceptor(
            AgentIdentityInterceptor(), // 从证书提取 Agent ID
            AuditLogInterceptor(),      // 审计日志
        ),
    )
    return server, nil
}
```

```go
// internal/agent/grpc/tls_config.go

// NewAgentTLSConfig 创建 Agent 端 mTLS 配置
func NewAgentTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
    cert, err := tls.LoadX509KeyPair(certFile, keyFile)
    if err != nil {
        return nil, fmt.Errorf("load agent cert: %w", err)
    }

    caCert, err := os.ReadFile(caFile)
    if err != nil {
        return nil, fmt.Errorf("read CA cert: %w", err)
    }
    caPool := x509.NewCertPool()
    caPool.AppendCertsFromPEM(caCert)

    return &tls.Config{
        Certificates: []tls.Certificate{cert},
        RootCAs:      caPool,       // 验证 Server 证书
        MinVersion:   tls.VersionTLS13,
    }, nil
}
```

### 2.4 Agent 身份提取拦截器

```go
// internal/server/grpc/interceptor_identity.go

import (
    "context"
    "crypto/x509"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"
    "google.golang.org/grpc/peer"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

type agentIDKey struct{}

// AgentIdentityInterceptor 从 mTLS 证书中提取 Agent ID
func AgentIdentityInterceptor() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (interface{}, error) {

        // 从 TLS 连接中提取对端证书
        p, ok := peer.FromContext(ctx)
        if !ok {
            return nil, status.Error(codes.Unauthenticated, "no peer info")
        }

        tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
        if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
            return nil, status.Error(codes.Unauthenticated, "no client certificate")
        }

        cert := tlsInfo.State.PeerCertificates[0]
        agentID := cert.Subject.CommonName // CN = Agent UUID

        // 校验 Agent 是否在注册白名单中
        if !isAgentRegistered(agentID) {
            return nil, status.Errorf(codes.PermissionDenied,
                "agent %s not registered", agentID)
        }

        // 将 Agent ID 注入 context
        ctx = context.WithValue(ctx, agentIDKey{}, agentID)
        return handler(ctx, req)
    }
}

// GetAgentID 从 context 获取 Agent ID
func GetAgentID(ctx context.Context) string {
    if id, ok := ctx.Value(agentIDKey{}).(string); ok {
        return id
    }
    return ""
}
```

### 2.5 Agent 注册白名单

```go
// internal/server/security/agent_registry.go

type AgentRegistry struct {
    db *gorm.DB
}

// Agent 注册表 — 只有注册过的 Agent 才能连接
type RegisteredAgent struct {
    ID        uint   `gorm:"primaryKey"`
    AgentUUID string `gorm:"uniqueIndex;size:128"`
    ClusterID string `gorm:"index;size:64"`
    Hostname  string `gorm:"size:256"`
    IPAddr    string `gorm:"size:45"` // 支持 IPv6
    Status    string `gorm:"size:16"` // active / revoked
    CertSN    string `gorm:"size:128"` // 证书序列号，用于吊销
    CreatedAt time.Time
    RevokedAt *time.Time
}

// Register 注册新 Agent（由管理员触发，不是 Agent 自注册）
func (r *AgentRegistry) Register(agent *RegisteredAgent) error {
    return r.db.Create(agent).Error
}

// IsRegistered 检查 Agent 是否已注册且未吊销
func (r *AgentRegistry) IsRegistered(agentUUID string) bool {
    var count int64
    r.db.Model(&RegisteredAgent{}).
        Where("agent_uuid = ? AND status = ?", agentUUID, "active").
        Count(&count)
    return count > 0
}

// Revoke 吊销 Agent（证书泄露/节点下线时调用）
func (r *AgentRegistry) Revoke(agentUUID string) error {
    now := time.Now()
    return r.db.Model(&RegisteredAgent{}).
        Where("agent_uuid = ?", agentUUID).
        Updates(map[string]interface{}{
            "status":     "revoked",
            "revoked_at": &now,
        }).Error
}
```

### 2.6 证书管理与轮转

```
证书生命周期管理：

1. 初始签发
   Admin → cfssl gencert → agent-{uuid}.pem/key
   → 随部署脚本分发到 Agent 节点

2. 自动轮转（推荐 cert-manager 方案）
   Agent 证书有效期 = 90 天
   cert-manager 在到期前 30 天自动续签
   Agent 监听证书文件变更（fsnotify），热加载新证书

3. 紧急吊销
   安全事件 → Admin 调用 Revoke API → Agent 被拒绝连接
   CRL（Certificate Revocation List）+ Agent Registry 双重检查
```

---

## 三、命令执行安全（核心）

### 3.1 威胁场景

管控平台最危险的能力是在 Agent 节点上执行 Shell 命令。如果命令内容可以被注入，攻击者就拿到了集群的 root 权限。

```
正常命令：/opt/tbds/bin/start_hdfs.sh --cluster=my-cluster
注入攻击：/opt/tbds/bin/start_hdfs.sh --cluster=my-cluster; rm -rf /
```

### 3.2 三层防御体系

```
┌──────────────────────────────────────────────────────┐
│ Layer 1: 命令模板化 — 参数不可自由拼接               │
│   ↓ 通过                                             │
│ Layer 2: 参数校验 — 白名单 + 正则 + 类型检查          │
│   ↓ 通过                                             │
│ Layer 3: 执行沙箱 — 限制执行环境和权限               │
│   ↓ 执行                                             │
│ + 审计日志: 记录每一条命令的完整上下文               │
└──────────────────────────────────────────────────────┘
```

### 3.3 Layer 1: 命令模板化

**核心原则**：永远不允许用户直接传入 Shell 命令字符串。所有命令必须是预定义模板 + 参数填充。

```go
// internal/server/security/command_template.go

// CommandTemplate 命令模板定义
type CommandTemplate struct {
    ID          string            `json:"id"`          // 如 "start_service"
    Binary      string            `json:"binary"`      // 可执行文件路径
    ArgsPattern []string          `json:"args_pattern"` // 参数模板
    AllowedParams map[string]ParamRule `json:"allowed_params"` // 参数校验规则
}

// ParamRule 参数校验规则
type ParamRule struct {
    Type    string `json:"type"`    // string / int / enum / path
    Pattern string `json:"pattern"` // 正则表达式
    Enum    []string `json:"enum"`  // 枚举值列表
    MaxLen  int    `json:"max_len"` // 最大长度
}

// 预定义的命令模板库
var CommandTemplates = map[string]CommandTemplate{
    "start_service": {
        ID:     "start_service",
        Binary: "/opt/tbds/bin/service_ctl.sh",
        ArgsPattern: []string{"start", "{{.ServiceName}}",
            "--cluster={{.ClusterID}}"},
        AllowedParams: map[string]ParamRule{
            "ServiceName": {Type: "enum",
                Enum: []string{"hdfs", "yarn", "hive", "kafka", "zookeeper"}},
            "ClusterID":   {Type: "string",
                Pattern: `^[a-zA-Z0-9_-]{1,64}$`, MaxLen: 64},
        },
    },
    "check_process": {
        ID:     "check_process",
        Binary: "/opt/tbds/bin/check_process.sh",
        ArgsPattern: []string{"{{.ProcessName}}"},
        AllowedParams: map[string]ParamRule{
            "ProcessName": {Type: "string",
                Pattern: `^[a-zA-Z0-9_.-]{1,128}$`, MaxLen: 128},
        },
    },
    "config_deploy": {
        ID:     "config_deploy",
        Binary: "/opt/tbds/bin/deploy_config.sh",
        ArgsPattern: []string{"--service={{.ServiceName}}",
            "--config-version={{.Version}}"},
        AllowedParams: map[string]ParamRule{
            "ServiceName": {Type: "enum",
                Enum: []string{"hdfs", "yarn", "hive", "kafka"}},
            "Version":     {Type: "string",
                Pattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, MaxLen: 20},
        },
    },
}
```

### 3.4 Layer 2: 参数安全校验

```go
// internal/server/security/command_validator.go

import (
    "fmt"
    "regexp"
    "strings"
)

// 危险字符黑名单 — 任何参数中出现这些字符都拒绝
var dangerousChars = []string{
    ";", "|", "&", "`", "$", "(", ")", "{", "}", "<", ">",
    "\n", "\r", "\\", "'", "\"",
}

// ValidateCommandParams 校验命令参数
func ValidateCommandParams(templateID string, params map[string]string) error {
    tmpl, ok := CommandTemplates[templateID]
    if !ok {
        return fmt.Errorf("unknown command template: %s", templateID)
    }

    for paramName, value := range params {
        rule, ok := tmpl.AllowedParams[paramName]
        if !ok {
            return fmt.Errorf("unexpected param: %s", paramName)
        }

        // 1. 检查危险字符（所有参数通用）
        for _, ch := range dangerousChars {
            if strings.Contains(value, ch) {
                return fmt.Errorf("param %s contains dangerous char: %s",
                    paramName, ch)
            }
        }

        // 2. 长度检查
        if rule.MaxLen > 0 && len(value) > rule.MaxLen {
            return fmt.Errorf("param %s exceeds max length %d",
                paramName, rule.MaxLen)
        }

        // 3. 类型特定校验
        switch rule.Type {
        case "enum":
            if !contains(rule.Enum, value) {
                return fmt.Errorf("param %s must be one of %v",
                    paramName, rule.Enum)
            }
        case "string":
            if rule.Pattern != "" {
                matched, _ := regexp.MatchString(rule.Pattern, value)
                if !matched {
                    return fmt.Errorf("param %s does not match pattern %s",
                        paramName, rule.Pattern)
                }
            }
        case "int":
            if _, err := strconv.Atoi(value); err != nil {
                return fmt.Errorf("param %s must be integer", paramName)
            }
        case "path":
            // 路径参数：禁止 .. 防止路径穿越
            if strings.Contains(value, "..") {
                return fmt.Errorf("param %s contains path traversal", paramName)
            }
        }
    }

    return nil
}
```

### 3.5 Layer 3: 执行沙箱

```go
// internal/agent/executor/sandbox.go

import (
    "context"
    "os/exec"
    "syscall"
    "time"
)

// SandboxConfig 沙箱配置
type SandboxConfig struct {
    MaxExecTime    time.Duration // 单命令最大执行时间，默认 30min
    MaxOutputBytes int64         // 最大输出大小，默认 10MB
    RunAsUser      string        // 执行用户（非 root）
    AllowedPaths   []string      // 允许访问的路径前缀
}

// ExecuteInSandbox 在受限环境中执行命令
func ExecuteInSandbox(ctx context.Context, cfg SandboxConfig,
    binary string, args []string) (*ExecResult, error) {

    // 1. 超时控制
    ctx, cancel := context.WithTimeout(ctx, cfg.MaxExecTime)
    defer cancel()

    cmd := exec.CommandContext(ctx, binary, args...)

    // 2. 以非 root 用户执行（如果配置了）
    if cfg.RunAsUser != "" {
        uid, gid := lookupUser(cfg.RunAsUser)
        cmd.SysProcAttr = &syscall.SysProcAttr{
            Credential: &syscall.Credential{
                Uid: uint32(uid),
                Gid: uint32(gid),
            },
            // 3. 创建新进程组，方便超时时 kill 整个进程树
            Setpgid: true,
        }
    }

    // 4. 限制输出大小
    stdout := &LimitedBuffer{MaxBytes: cfg.MaxOutputBytes}
    stderr := &LimitedBuffer{MaxBytes: cfg.MaxOutputBytes}
    cmd.Stdout = stdout
    cmd.Stderr = stderr

    // 5. 清理环境变量（移除敏感信息）
    cmd.Env = sanitizeEnv(os.Environ())

    err := cmd.Run()

    return &ExecResult{
        ExitCode: cmd.ProcessState.ExitCode(),
        Stdout:   stdout.String(),
        Stderr:   stderr.String(),
    }, err
}

// sanitizeEnv 清理环境变量，移除敏感信息
func sanitizeEnv(env []string) []string {
    blocked := map[string]bool{
        "AWS_SECRET_ACCESS_KEY": true,
        "DB_PASSWORD":          true,
        "REDIS_PASSWORD":       true,
    }
    var clean []string
    for _, e := range env {
        key := strings.SplitN(e, "=", 2)[0]
        if !blocked[key] {
            clean = append(clean, e)
        }
    }
    return clean
}
```

---

## 四、API 安全

### 4.1 RBAC 权限模型

```go
// internal/server/security/rbac.go

// Role 角色定义
type Role string

const (
    RoleAdmin    Role = "admin"    // 超级管理员：所有操作
    RoleOperator Role = "operator" // 运维人员：创建/执行 Job
    RoleViewer   Role = "viewer"   // 只读用户：查看 Job 状态
    RoleAgent    Role = "agent"    // Agent：只能 Fetch/Report
)

// Permission 权限定义
type Permission struct {
    Resource string // job / action / agent / cluster
    Action   string // create / read / execute / delete
}

// RolePermissions 角色 → 权限映射
var RolePermissions = map[Role][]Permission{
    RoleAdmin: {
        {Resource: "*", Action: "*"}, // 全部权限
    },
    RoleOperator: {
        {Resource: "job", Action: "create"},
        {Resource: "job", Action: "read"},
        {Resource: "action", Action: "read"},
        {Resource: "cluster", Action: "read"},
    },
    RoleViewer: {
        {Resource: "job", Action: "read"},
        {Resource: "action", Action: "read"},
        {Resource: "cluster", Action: "read"},
    },
    RoleAgent: {
        {Resource: "action", Action: "fetch"},
        {Resource: "action", Action: "report"},
        {Resource: "heartbeat", Action: "send"},
    },
}
```

### 4.2 JWT Token 签发与验证

```go
// internal/server/security/jwt.go

import (
    "time"
    "github.com/golang-jwt/jwt/v5"
)

type Claims struct {
    jwt.RegisteredClaims
    UserID    string `json:"uid"`
    Role      Role   `json:"role"`
    ClusterID string `json:"cid,omitempty"` // Agent 绑定的集群
}

// IssueToken 签发 JWT
func IssueToken(secret []byte, userID string, role Role, ttl time.Duration) (string, error) {
    now := time.Now()
    claims := Claims{
        RegisteredClaims: jwt.RegisteredClaims{
            IssuedAt:  jwt.NewNumericDate(now),
            ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
            Issuer:    "tbds-control",
        },
        UserID: userID,
        Role:   role,
    }
    token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
    return token.SignedString(secret)
}

// VerifyToken 验证 JWT
func VerifyToken(secret []byte, tokenStr string) (*Claims, error) {
    token, err := jwt.ParseWithClaims(tokenStr, &Claims{},
        func(t *jwt.Token) (interface{}, error) {
            return secret, nil
        })
    if err != nil {
        return nil, err
    }
    claims, ok := token.Claims.(*Claims)
    if !ok || !token.Valid {
        return nil, fmt.Errorf("invalid token")
    }
    return claims, nil
}
```

### 4.3 API 防重放（Nonce + Timestamp）

```go
// internal/server/middleware/anti_replay.go

// AntiReplayMiddleware 防重放攻击
// 原理：每个请求携带 timestamp + nonce，Server 验证时效性 + 唯一性
func AntiReplayMiddleware(rdb *redis.Client) gin.HandlerFunc {
    return func(c *gin.Context) {
        ts := c.GetHeader("X-Timestamp")
        nonce := c.GetHeader("X-Nonce")

        if ts == "" || nonce == "" {
            c.AbortWithStatusJSON(401, gin.H{"error": "missing auth headers"})
            return
        }

        // 1. 验证时间戳（5 分钟内有效）
        reqTime, _ := strconv.ParseInt(ts, 10, 64)
        if abs(time.Now().Unix()-reqTime) > 300 {
            c.AbortWithStatusJSON(401, gin.H{"error": "request expired"})
            return
        }

        // 2. 验证 nonce 唯一性（Redis SET NX，TTL = 5 分钟）
        key := fmt.Sprintf("nonce:%s", nonce)
        ok, _ := rdb.SetNX(c, key, "1", 5*time.Minute).Result()
        if !ok {
            c.AbortWithStatusJSON(401, gin.H{"error": "duplicate request"})
            return
        }

        c.Next()
    }
}
```

---

## 五、数据安全

### 5.1 敏感信息加密存储

```go
// internal/pkg/crypto/encrypt.go

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
    "io"
)

// Encrypt AES-256-GCM 加密
func Encrypt(plaintext string, key []byte) (string, error) {
    block, err := aes.NewCipher(key) // key 必须 32 字节
    if err != nil {
        return "", err
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }

    nonce := make([]byte, gcm.NonceSize())
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return "", err
    }

    ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
    return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt AES-256-GCM 解密
func Decrypt(ciphertextB64 string, key []byte) (string, error) {
    ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
    if err != nil {
        return "", err
    }

    block, err := aes.NewCipher(key)
    if err != nil {
        return "", err
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }

    nonceSize := gcm.NonceSize()
    nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]

    plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
    if err != nil {
        return "", err
    }
    return string(plaintext), nil
}
```

### 5.2 配置文件安全

```ini
; config.ini — 敏感字段加密存储

[mysql]
host = 127.0.0.1
port = 3306
user = woodpecker
; 密码使用 AES-256-GCM 加密，前缀 ENC() 标识
password = ENC(aGVsbG8gd29ybGQ=...)

[redis]
host = 127.0.0.1
port = 6379
password = ENC(c2VjcmV0MTIz...)

; 加密密钥来源（优先级从高到低）：
; 1. 环境变量 TBDS_MASTER_KEY
; 2. K8s Secret 挂载 /etc/tbds/master.key
; 3. 本地文件 ~/.tbds/master.key（仅开发环境）
```

```go
// 配置加载时自动解密 ENC() 字段
func LoadConfig(path string) (*Config, error) {
    cfg, _ := ini.Load(path)

    masterKey := getMasterKey() // 从环境变量/Secret 获取

    mysqlPwd := cfg.Section("mysql").Key("password").String()
    if strings.HasPrefix(mysqlPwd, "ENC(") {
        encrypted := mysqlPwd[4 : len(mysqlPwd)-1]
        mysqlPwd, _ = crypto.Decrypt(encrypted, masterKey)
    }
    // ...
}
```

---

## 六、审计日志

### 6.1 审计事件定义

```go
// internal/server/audit/audit.go

type AuditEvent struct {
    ID        string    `json:"id"`
    Timestamp time.Time `json:"timestamp"`
    Actor     string    `json:"actor"`      // 操作者（userID / agentUUID）
    ActorType string    `json:"actor_type"` // user / agent / system
    Action    string    `json:"action"`     // create_job / fetch_action / revoke_agent
    Resource  string    `json:"resource"`   // job:12345 / agent:uuid-xxx
    Detail    string    `json:"detail"`     // 操作详情（JSON）
    SourceIP  string    `json:"source_ip"`
    TraceID   string    `json:"trace_id"`   // 关联链路追踪
    Result    string    `json:"result"`     // success / denied / error
}

// AuditLogger 审计日志写入器
type AuditLogger struct {
    writer io.Writer // 写入独立的审计日志文件
    logger *zap.Logger
}

func (a *AuditLogger) Log(event AuditEvent) {
    event.ID = uuid.NewString()
    event.Timestamp = time.Now()

    // 结构化日志输出（便于 ELK 采集）
    a.logger.Info("audit",
        zap.String("event_id", event.ID),
        zap.String("actor", event.Actor),
        zap.String("action", event.Action),
        zap.String("resource", event.Resource),
        zap.String("result", event.Result),
        zap.String("source_ip", event.SourceIP),
        zap.String("trace_id", event.TraceID),
    )
}
```

### 6.2 审计拦截器

```go
// AuditLogInterceptor gRPC 审计拦截器
func AuditLogInterceptor() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (interface{}, error) {

        agentID := GetAgentID(ctx)
        sourceIP := extractClientIP(ctx)

        resp, err := handler(ctx, req)

        result := "success"
        if err != nil {
            result = "error"
        }

        auditLogger.Log(AuditEvent{
            Actor:     agentID,
            ActorType: "agent",
            Action:    info.FullMethod,
            SourceIP:  sourceIP,
            TraceID:   trace.SpanFromContext(ctx).SpanContext().TraceID().String(),
            Result:    result,
        })

        return resp, err
    }
}
```

### 6.3 审计日志存储与查询

```
审计日志流转：

Application → zap structured log → /var/log/tbds/audit.log
    → Filebeat → Logstash → Elasticsearch (专用索引 audit-*)
    → Kibana Dashboard（操作时间线 + 异常操作告警）

保留策略：
- Hot:   30 天（SSD，快速查询）
- Warm:  90 天（HDD，压缩存储）
- Cold:  365 天（归档，合规要求）
```

---

## 七、安全体系全景图

```
┌─────────────────────────────────────────────────────────────────────┐
│                       安全体系全景图                                  │
│                                                                      │
│  ┌─ 网络层 ─────────────────────────────────────────────────────┐   │
│  │  mTLS 双向认证（TLS 1.3）                                    │   │
│  │  Agent ←→ Server 加密通信                                    │   │
│  │  证书轮转（90 天） + 紧急吊销                                 │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─ 应用层 ─────────────────────────────────────────────────────┐   │
│  │  Agent 注册白名单 + 证书 CN 身份提取                          │   │
│  │  JWT Token 鉴权（RBAC 四角色）                                │   │
│  │  API 防重放（Nonce + Timestamp）                              │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─ 命令层 ─────────────────────────────────────────────────────┐   │
│  │  命令模板化（禁止自由 Shell 拼接）                            │   │
│  │  参数白名单 + 正则 + 危险字符过滤                             │   │
│  │  执行沙箱（超时 + 非 root + 输出限制）                        │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─ 数据层 ─────────────────────────────────────────────────────┐   │
│  │  敏感信息 AES-256-GCM 加密存储                                │   │
│  │  Master Key 从环境变量/K8s Secret 获取                        │   │
│  │  配置文件 ENC() 自动解密                                      │   │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  ┌─ 审计层 ─────────────────────────────────────────────────────┐   │
│  │  全操作审计日志（who + when + what + result）                 │   │
│  │  ELK 采集 + 365 天保留                                       │   │
│  │  异常操作告警（非工作时间操作 / 高频拒绝等）                  │   │
│  └──────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 八、与 SaltStack 安全模型对比

| 维度 | 本项目 | SaltStack |
|------|--------|-----------|
| 认证 | mTLS 双向证书 | AES Key 自动协商（CVE-2020-11651 暴露了缺陷） |
| 命令安全 | 模板化 + 参数白名单 | State 文件 + Pillar（灵活但更易注入） |
| 权限控制 | RBAC 四角色 | External Auth + ACL（功能更丰富但配置复杂） |
| 审计 | 全操作审计 + ELK | Event System + Returner |
| 证书管理 | cfssl 签发 + cert-manager 轮转 | Salt Master 自动签发（安全性较弱） |

**面试表达**：
> "SaltStack 在 2020 年爆了一个 Critical 漏洞（CVE-2020-11651），根因是认证绕过——任何人都能连上 Salt Master 执行命令。我从这个事故中汲取了教训：第一，Agent 必须强认证（mTLS 证书，不是共享密钥）；第二，命令执行必须模板化，不允许自由 Shell 拼接；第三，所有操作都有审计日志。相当于从 SaltStack 的失败中学到了正确的安全架构。"

---

## 九、面试 Q&A

### Q1: "Agent 怎么防伪造？如果有人在非法节点上部署了一个 Agent 会怎样？"

> "三层防护。第一层是 mTLS 双向认证——Server 会验证 Agent 的客户端证书，证书必须是我们内部 CA 签发的，自签证书连不上。第二层是 Agent 注册白名单——即使证书合法，Agent UUID 如果不在注册表里也会被拒绝。第三层是审计——任何连接尝试都有日志，异常连接会触发告警。"

### Q2: "命令注入怎么防？如果有人在 Job 参数里注入了 rm -rf / 怎么办？"

> "核心原则：永远不拼接 Shell 命令。所有命令都是预定义模板——比如 start_service 模板只允许传入 ServiceName（枚举值）和 ClusterID（正则校验）。参数里出现分号、管道符、反引号等危险字符直接拒绝。即使通过了参数校验，Agent 端还有执行沙箱——非 root 用户执行、超时自动 kill、输出大小限制。"

### Q3: "密码、密钥这些敏感信息怎么管理的？"

> "配置文件里的敏感字段用 AES-256-GCM 加密存储，标记为 ENC(xxx)。解密密钥不在配置文件里——生产环境从 K8s Secret 挂载，开发环境从环境变量读取。这样即使配置文件泄露，攻击者也拿不到明文密码。"

### Q4: "出了安全事故怎么溯源？"

> "全操作审计日志。每条日志记录五个要素：谁（actor）、什么时候（timestamp）、做了什么（action）、对哪个资源（resource）、结果如何（result）。日志走 ELK 管线，保留 365 天。关键操作（如 Agent 注册/吊销、高危命令执行）还有实时告警。审计日志里还嵌入了 traceId，可以直接跳到 Jaeger 看完整链路。"

---

## 十、实施计划

| 阶段 | 内容 | 工时 | 优先级 |
|------|------|------|--------|
| **Phase 1** | mTLS 双向认证 + Agent 注册白名单 | 2 天 | P0 |
| **Phase 2** | 命令模板化 + 参数校验 | 1 天 | P0 |
| **Phase 3** | 审计日志框架 | 1 天 | P0 |
| **Phase 4** | RBAC + JWT API 鉴权 | 1 天 | P1 |
| **Phase 5** | 数据加密 + 密钥管理 | 0.5 天 | P1 |
| **Phase 6** | 执行沙箱 + 防重放 | 0.5 天 | P2 |

**新增依赖**：

| 包 | 用途 |
|----|------|
| `github.com/golang-jwt/jwt/v5` | JWT 签发与验证 |
| `github.com/cloudflare/cfssl` | 证书签发工具（CLI，不引入代码） |
