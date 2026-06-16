# 配置路径

从 **Reasonix v1.8.1** 开始，Reasonix 使用一个用户可见的全局目录存放配置和用户状态。CLI 与桌面端共用这个目录。

## Reasonix Home

| 平台 | Reasonix home |
| --- | --- |
| macOS | `~/.reasonix` |
| Linux | `~/.reasonix` |
| Windows | `%APPDATA%\reasonix` |

可以设置 `REASONIX_HOME` 覆盖 Reasonix home，主要用于测试、CI 或便携安装。普通用户通常不需要设置。

## 目录内容

| 数据 | 路径 |
| --- | --- |
| 全局配置 | `<Reasonix home>/config.toml` |
| 全局 credentials | `<Reasonix home>/credentials` |
| 全局斜杠命令 | `<Reasonix home>/commands/` |
| 全局 skills | `<Reasonix home>/skills/` |
| 全局 hooks | `<Reasonix home>/settings.json` |
| hooks 信任状态 | `<Reasonix home>/trust.json` |
| 会话 | `<Reasonix home>/sessions/` |
| 归档 | `<Reasonix home>/archive/` |
| 记忆 | `<Reasonix home>/memory/` 与 `<Reasonix home>/projects/` |

缓存仍放在系统缓存目录，例如 macOS 的 `~/Library/Caches/reasonix`、
Linux 的 `$XDG_CACHE_HOME/reasonix` 或 `~/.cache/reasonix`、Windows 的
`%LOCALAPPDATA%\reasonix\cache`。可以设置 `REASONIX_CACHE_HOME` 覆盖缓存根目录。

## 配置优先级

运行时配置按下面顺序解析：

```text
命令行参数
> 项目 ./reasonix.toml
> 全局 <Reasonix home>/config.toml
> 兼容读取的旧全局配置
> 内置默认值
```

写配置时始终写入新的全局路径：

```text
macOS/Linux: ~/.reasonix/config.toml
Windows:     %APPDATA%\reasonix\config.toml
```

## 旧路径迁移

从 **v1.8.1** 开始，Reasonix 启动时会在第一次加载配置前自动检查旧路径。迁移是同步、一次性、非破坏性的：旧文件会被复制或转换到 Reasonix home，原文件保留。

旧配置来源包括：

```text
~/Library/Application Support/reasonix/config.toml
~/.config/reasonix/config.toml
~/.reasonix/reasonix.toml
~/.reasonix/config.json
```

旧 credentials 和 sessions 也会在新文件不存在时复制到 Reasonix home。若新的全局配置已经存在，则新配置优先；旧配置只作为兼容 fallback 保留。
