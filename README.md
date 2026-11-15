# naive-cacher

`naive-cacher` 是一个面向 VPC 内镜像源场景的 Go 反向代理，放在 nginx 后面：

```text
nginx -> naive-cacher -> upstream registry / Go proxy / blob storage
```

它按入站 `Host` 把不同域名路由到不同上游，不需要 `/go/` 这类路径前缀。它可以跟随上游 `302` 等重定向，并按路径覆盖超时时间和 `Cache-Control`。真正的磁盘缓存由 nginx 完成，`naive-cacher` 本身不落盘。

## 快速开始

```bash
go build -o naive-cacher .
cp config.example.yaml config.yaml
./naive-cacher -config config.yaml
```

默认监听 `:8080`。nginx 必须保留原始域名：

```nginx
proxy_set_header Host $host;
```

完整 nginx 缓存配置见 `nginx.example.conf`。

## 配置示例

```yaml
listen: ":8080"

sites:
  - hosts:
      - go.example.internal
      - goproxy.example.internal
    upstream: https://proxy.golang.org
    follow_redirects: true
    max_redirects: 10
    timeout: 30s
    cache_control: public, max-age=3600
    paths:
      - regex: '\.zip$'
        timeout: 5m
        cache_control: public, max-age=315360000, immutable
      - regex: '/@v/'
        timeout: 30s
        cache_control: public, max-age=60

  - hosts:
      - registry.example.internal
    upstream: https://registry-1.docker.io
    timeout: 60s
    paths:
      - regex: '/blobs/sha256:[a-f0-9]+$'
        cache_control: public, max-age=315360000, immutable
```

## 域名路由

- `hosts` 支持精确域名、`*.example.internal` 通配符和 `*` 兜底。
- 匹配优先级：精确域名 > 通配符 > `*`。通配符按配置顺序匹配。
- 域名忽略大小写、端口和末尾的 `.`。
- 未匹配任何站点时返回 `404`。
- 请求路径和查询参数原样转发；`upstream` 可以带基础路径，例如 `https://registry.example.internal/prefix`。

## 路径规则

每个站点可以配置 `paths`，按顺序首条命中生效。每条规则必须且只能使用一种匹配方式：

- `prefix`: 从路径开头匹配的前缀，例如 `/v2/`。
- `regex`: 正则表达式，例如 `\.zip$` 或 `/@v/`。

规则可以覆盖站点级 `timeout` 和 `cache_control`。`cache_control` 为空字符串时会删除上游返回的 `Cache-Control`；不配置则继承站点级设置，站点级也不配置则保留上游原值。

## 超时语义

`timeout` 限制的是“拿到上游最终响应头”的时间，包含连接、TLS、重定向链和等待响应头，不限制后续 blob  body 传输。因此大文件不会因为下载时间长而被固定超时切断。设置为 `0s` 可禁用该超时。

## 重定向

`follow_redirects` 默认 `true`，`max_redirects` 默认 `10`。关闭后会把上游 `302` 和 `Location` 原样返回给 nginx。跨主机重定向时，Go HTTP 客户端会避免把 `Authorization`、`Cookie` 等敏感头带到无关目标。

## nginx 缓存

`naive-cacher` 只负责设置响应头，nginx 负责缓存。示例配置使用：

```nginx
proxy_cache_key "$scheme$request_method$host$request_uri";
proxy_cache_valid 200 302 10m;
proxy_ignore_headers Set-Cookie Expires;
```

当上游或路径规则返回 `Cache-Control: public, max-age=315360000, immutable` 时，nginx 会按该值缓存约 10 年。`proxy_cache_valid` 只是没有缓存头时的兜底。

## Docker

```bash
docker build -t naive-cacher .
docker run --rm -p 8080:8080 -v "$PWD/config.yaml:/etc/naive-cacher/config.yaml:ro" naive-cacher
```

## 测试

```bash
go test ./...
go vet ./...
```
