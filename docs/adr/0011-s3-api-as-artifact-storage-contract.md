---
status: accepted
---

# 以 S3 API 作为工件存储契约

Artifact 的字节内容存放在 S3 兼容对象存储中，平台只依赖 S3 协议（通过 minio-go 客户端），元数据存放在 PostgreSQL（ADR-0007）。私有化部署由客户选择任何 S3 兼容实现（MinIO 企业版、Ceph RGW、SeaweedFS 等）；单二进制开发模式另有本地文件系统实现。

原计划开发环境使用 MinIO，但其社区版容器镜像已无法从 Docker Hub 与 quay.io 获取，因此开发环境改用 SeaweedFS。这只影响 `docker-compose.yml`，不影响代码。

## Consequences

- 所有对象存储实现必须通过 `blobtest` 一致性套件。
- 设备与沙箱经 yanshi API 上传下载工件，不直接访问对象存储；对象存储无需对外暴露，也无需预签名 URL。
