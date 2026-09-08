-- +goose Up

-- FR17 makes a transaction reference spendable exactly once system-wide, and
-- payments_transaction_reference_key enforces that. It is case-sensitive, so
-- resubmitting the same reference in different case would slip past it.
--
-- Case folding cannot be done in the application instead: a CBE new-platform
-- token is a case-sensitive URL path segment and a Telebirr reference is
-- interpolated into a lookup URL as printed, so the stored value has to stay
-- verbatim. The uniqueness check is what folds.
CREATE UNIQUE INDEX payments_transaction_reference_ci_idx
    ON payments (lower(transaction_reference));

-- +goose Down
DROP INDEX payments_transaction_reference_ci_idx;
