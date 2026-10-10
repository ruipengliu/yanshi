-- 推送设备（docs/design/m3-duplex-channel.md §9）：EndUser 登记了推送的设备与最近在前台使用的时间。
-- 属于 EndUser 数据（不属于任何 Session），注销账号时删除。
CREATE TABLE push_devices (
    business_line text        NOT NULL,
    end_user      text        NOT NULL,
    device_id     text        NOT NULL,
    label         text        NOT NULL,
    kind          text        NOT NULL,
    platform      text        NOT NULL,
    token         text        NOT NULL,
    last_active   timestamptz NOT NULL,
    PRIMARY KEY (business_line, end_user, device_id)
);
