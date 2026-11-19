CREATE TABLE end_user_consents (
    business_line text NOT NULL,
    end_user text NOT NULL,
    kind text NOT NULL,
    granted_at timestamptz NOT NULL,
    PRIMARY KEY (business_line, end_user, kind)
);
