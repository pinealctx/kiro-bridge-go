# 设计文档: Kiro Gateway Go 重写

## 概述

Kiro Gateway 是一个 API 网关，将 Kiro CLI 的 CodeWhisperer 后端封装为标准的 OpenAI 和 Anthropic 兼容 API 端点。本设计文档描述将现有 Python (FastAPI) 实现完整重写为 Golang 的技术方案。

Go 重写的核心目标：
- 功能完全对等：所有 Python 版本的功能必须 1:1 移植
- 性能提升：利用 Go 的并发模型和编译优势，降低延迟、减少内存占用
- 部署简化：单一静态二进制，无需 Python 运行时和依赖管理
- 可维护性：利用 Go 的强类型系统和接口抽象，提升代码质量

请求处理流程保持不变：客户端发送 OpenAI/Anthropic 格式请求 → 协议转换为 CodeWhisperer 格式 → 流式调用 CW 后端 → EventStream 解析 → 响应清洗 → 转换回客户端协议格式。

## 架构

### 整体架构

```mermaid
graph TD
    Client[客户端] --> Router[HTTP Router / chi]
    Router --> AuthMW[认证中间件]
    AuthMW --> MetricsMW[指标中间件]
    MetricsMW --> OpenAI[OpenAI Routes]
    MetricsMW --> Anthropic[Anthropic Routes]
    MetricsMW --> Health[Health / Metrics]
    
    OpenAI --> Converter[协议转换器]
    Anthropic --> AnthropicConv[Anthropic→OpenAI 转换]
    AnthropicConv --> Converter
    
    Converter --> CWClient[CW HTTP Client]
    CWClient --> EventParser[EventStream 解析器]
    EventParser --> Sanitizer[响应清洗器]
    Sanitizer --> OpenAI
    Sanitizer --> Anthropic
    
    CWClient --> TokenMgr[Token 管理器]
    TokenMgr --> SQLite[(Kiro CLI SQLite)]
    
    Config[配置管理器] --> Router
    Config --> CWClient
    Config --> TokenMgr
    
    Stats[统计收集器] --> OpenAI
    Stats --> Anthropic
    Metrics[Prometheus 指标] --> MetricsMW
```

### 请求处理时序图

```mermaid
sequenceDiagram
    participant C as 客户端
    participant R as Router
    participant MW as 中间件链
    participant H as Route Handler
    participant Conv as Converter
    participant TM as TokenManager
    participant CW as CW Client
    participant EP as EventStream Parser
    participant San as Sanitizer

    C->>R: POST /v1/chat/completions
    R->>MW: 认证 + 指标
    MW->>H: 请求分发
    H->>TM: get_access_token()
    TM-->>H: Bearer token
    H->>Conv: openai_to_codewhisperer()
    Conv-->>H: CW 请求体
    H->>CW: POST /generateAssistantResponse (流式)
    
    loop EventStream 消息
        CW-->>EP: 二进制 EventStream 块
        EP-->>San: 解析后的事件
        San-->>H: 清洗后的内容
        H-->>C: SSE data chunk
    end
    
    H-->>C: data: [DONE]
```

### Token 刷新时序图

```mermaid
sequenceDiagram
    participant H as Handler
    participant TM as TokenManager
    participant DB as SQLite DB
    participant MS as Microsoft OAuth2
    participant IdC as AWS IdC

    H->>TM: GetAccessToken()
    
    alt 首次加载
        TM->>DB: 读取 kirocli:external-idp:token
        alt External IdP 存在
            DB-->>TM: token 数据
            TM->>TM: 解析 External IdP token
        else 回退 Legacy
            TM->>DB: 读取 kirocli:odic:token
            DB-->>TM: token 数据
            TM->>TM: 解析 Legacy IdC token
        end
    end
    
    alt Token 即将过期 (< 5分钟)
        alt External IdP
            TM->>MS: POST token_endpoint (refresh_token)
            MS-->>TM: 新 access_token
        else Legacy IdC
            TM->>IdC: POST /token (refresh_token)
            IdC-->>TM: 新 accessToken
        end
    end
    
    TM-->>H: 有效的 access_token
```

