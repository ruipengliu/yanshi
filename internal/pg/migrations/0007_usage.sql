-- 用量（docs/design/m4-quota-usage.md §4，ADR-0019）：独立于 Event 日志长期保存，不含 Session 与内容；
-- 注销 EndUser 时其标识改为匿名值 '~'。
CREATE TABLE usage_entries (
    id             text        PRIMARY KEY,
    business_line  text        NOT NULL,
    end_user       text        NOT NULL,
    kind           text        NOT NULL,
    model          text        NOT NULL,
    input_tokens   bigint      NOT NULL,
    output_tokens  bigint      NOT NULL,
    sandbox_millis bigint      NOT NULL,
    cost           bigint      NOT NULL,
    at             timestamptz NOT NULL
);
CREATE INDEX usage_entries_business_line ON usage_entries (business_line, at);
CREATE INDEX usage_entries_end_user ON usage_entries (business_line, end_user);
CREATE INDEX usage_entries_at ON usage_entries (at);

-- 按小时（UTC）的聚合，配额检查只读它；end_user = '' 的行是业务线合计。
CREATE TABLE usage_hourly (
    business_line  text        NOT NULL,
    end_user       text        NOT NULL,
    hour           timestamptz NOT NULL,
    cost           bigint      NOT NULL,
    input_tokens   bigint      NOT NULL,
    output_tokens  bigint      NOT NULL,
    sandbox_millis bigint      NOT NULL,
    PRIMARY KEY (business_line, end_user, hour)
);
CREATE INDEX usage_hourly_hour ON usage_hourly (hour);
