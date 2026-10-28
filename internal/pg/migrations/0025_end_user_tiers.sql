CREATE TABLE end_user_tiers (
    business_line text NOT NULL,
    end_user text NOT NULL,
    tier text NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (business_line, end_user)
);
