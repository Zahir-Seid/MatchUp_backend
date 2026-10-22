-- name: CreateUser :one
INSERT INTO users (full_name, email, password_hash, age, phone, role)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg(role)::text, 'player'))
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: LockActiveAdmins :many
SELECT id FROM users WHERE role = 'admin' AND status = 'active' FOR UPDATE;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: MarkEmailVerified :exec
UPDATE users SET email_verified_at = now() WHERE id = $1 AND email_verified_at IS NULL;

-- name: SetUserStatus :one
UPDATE users SET status = $2 WHERE id = $1 RETURNING *;

-- name: ListUsers :many
SELECT * FROM users
WHERE (sqlc.narg(search)::text IS NULL
       OR full_name ILIKE '%' || sqlc.narg(search) || '%'
       OR email::text ILIKE '%' || sqlc.narg(search) || '%')
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;

-- name: CountUsers :one
SELECT count(*) FROM users
WHERE (sqlc.narg(search)::text IS NULL
       OR full_name ILIKE '%' || sqlc.narg(search) || '%'
       OR email::text ILIKE '%' || sqlc.narg(search) || '%')
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status));

-- name: CreateOAuthIdentity :one
INSERT INTO oauth_identities (user_id, provider, provider_subject)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByOAuthSubject :one
SELECT u.* FROM users u
JOIN oauth_identities oi ON oi.user_id = u.id
WHERE oi.provider = $1 AND oi.provider_subject = $2;

-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, token_hash, expires_at, user_agent)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM refresh_tokens WHERE token_hash = $1;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- Rotation: the outgoing token is revoked and linked to its replacement in
-- one statement, so a replayed old token is identifiable as reuse.
-- name: RotateRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now(), rotated_to = $2
WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeAllUserRefreshTokens :execrows
UPDATE refresh_tokens SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
DELETE FROM refresh_tokens WHERE expires_at < now() - interval '30 days';

-- name: CreatePasswordResetToken :one
INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetPasswordResetTokenByHash :one
SELECT * FROM password_reset_tokens WHERE token_hash = $1;

-- name: ConsumePasswordResetToken :execrows
UPDATE password_reset_tokens SET used_at = now()
WHERE id = $1 AND used_at IS NULL AND expires_at > now();

-- name: InvalidateUserPasswordResetTokens :execrows
UPDATE password_reset_tokens SET used_at = now()
WHERE user_id = $1 AND used_at IS NULL;

-- name: CreateEmailVerificationToken :one
INSERT INTO email_verification_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetEmailVerificationTokenByHash :one
SELECT * FROM email_verification_tokens WHERE token_hash = $1;

-- name: ConsumeEmailVerificationToken :execrows
UPDATE email_verification_tokens SET used_at = now()
WHERE id = $1 AND used_at IS NULL AND expires_at > now();

-- name: InvalidateUserEmailVerificationTokens :execrows
UPDATE email_verification_tokens SET used_at = now()
WHERE user_id = $1 AND used_at IS NULL;

-- Role changes are audited by the caller; the query itself only guards against
-- writing a role the CHECK constraint would reject anyway.
-- name: SetUserRole :one
UPDATE users SET role = $2 WHERE id = $1 AND role <> $2 RETURNING *;
