# 安全策略 / Security Policy

## 报告漏洞 / Reporting a vulnerability

本仓库是私有仓库。请**不要**在 issue、讨论或 PR 里描述安全问题，即使只有协作者能看到。请通过 GitHub Security Advisories 私下报告：

This is a private repository. Please **do not** describe security problems in issues, discussions or pull requests, even though only collaborators can see them. Report them privately through GitHub Security Advisories:

**<https://github.com/AMA-Link/armadra-cloud/security/advisories/new>**

报告里请写明 / Please include:

- 受影响的版本、组件与模式（中继 personal / saas、控制面、协议包）以及部署方式 / Affected version, component and mode (relay personal / saas, control plane, protocol package) and deployment
- 问题类型与影响，例如令牌伪造或重放、越权访问源或分享链接、流转错源、凭据或流正文泄露 / Type and impact, for example token forgery or replay, unauthorized access to sources or share links, forwarding to the wrong source, leaks of credentials or stream bodies
- 复现步骤或概念验证 / Steps to reproduce or a proof of concept
- 你建议的修复（可选） / A suggested fix (optional)

报告里不要附真实凭据、签名钥或 `state.json`；需要演示时请使用测试账号与本地自签部署。

Do not include real credentials, signing keys or `state.json`; use test accounts and a local self-signed deployment when demonstrating.

## 处理流程 / Process

1. 我们会在 7 天内确认收到。 / We acknowledge the report within 7 days.
2. 确认问题后在私有 advisory 里协同修复，并告知预计的发布时间。 / Once confirmed, we work on a fix in the private advisory and share the expected release timeline.
3. 修复以新版本镜像发布；涉及协议包时同步通知 Armadra 客户端。你愿意的话会在 advisory 里致谢。 / The fix ships as new images; when the protocol package is affected, the Armadra client is updated in step. We credit you in the advisory if you wish.

## 支持的版本 / Supported versions

只有最新发布版（`ghcr.io/ama-link/armadra-relay`、`ghcr.io/ama-link/armadra-cloud` 的最新标签与最新的 `@armadra/platform-protocol`）会收到安全修复。 / Only the latest release (the latest tags of `ghcr.io/ama-link/armadra-relay` and `ghcr.io/ama-link/armadra-cloud`, and the latest `@armadra/platform-protocol`) receives security fixes.

## 范围 / Scope

范围内：本仓库的中继（`go/`，personal 与 saas 模式；`apps/relay-workers` 与 `packages/relay-runtime-workers`，Cloudflare Workers 上的中继）、控制面（`apps/cloud`）、协议包（`packages/platform-protocol`）、中继内核与控制核（`packages/relay-kernel`、`packages/control-core`）、共享库（`packages/cloud-shared`），以及 `deploy/` 下的参考部署与镜像构建。

In scope: the relay (`go/`, personal and saas modes; `apps/relay-workers` and `packages/relay-runtime-workers`, the relay on Cloudflare Workers), the control plane (`apps/cloud`), the protocol package (`packages/platform-protocol`), the relay kernel and control core (`packages/relay-kernel`, `packages/control-core`), the shared library (`packages/cloud-shared`), and the reference deployments and image builds under `deploy/`.

范围外：Armadra 客户端（桌面、服务器壳、网页、手机端）的问题请向 Armadra 仓库报告；PostgreSQL、Redis、反向代理、ACME 服务等第三方组件自身的问题请向对应项目报告；自行修改过的部署配置导致的暴露不在范围内。

Out of scope: problems in the Armadra clients (desktop, server shell, web, mobile) belong to the Armadra repository; problems in third-party components such as PostgreSQL, Redis, reverse proxies or ACME services belong to the respective projects; exposure caused by self-modified deployment configuration is out of scope.
