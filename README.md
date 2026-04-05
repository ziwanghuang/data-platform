# data-platform


## 🚢 部署到远端服务器

```bash
# rsync 推送（自动排除不需要的文件）
rsync -avz \
    --exclude='.git/' \
    --exclude='.github/' \
    --exclude='__pycache__/' \
    --exclude='.workbuddy' \
    --exclude='vendor' \
    --exclude='python/.venv' \
    /Users/ziwh666/GitHub/data-platform \
    root@182.43.22.165:/data/github/

# 服务器上拉取最新代码
git fetch origin && git reset --hard origin/master
```

> 💡 **免密推送**：建议先配置 SSH 密钥认证，执行一次 `ssh-copy-id root@your-server-ip` 即可免密。