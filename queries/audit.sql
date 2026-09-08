-- name: CreateAuditLog :one
INSERT INTO audit_logs (actor_user_id, action, target_type, target_id, reason, metadata)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.narg(metadata)::jsonb, '{}'::jsonb))
RETURNING *;

-- name: ListAuditLogs :many
SELECT sqlc.embed(a), u.full_name AS actor_name, u.email AS actor_email
FROM audit_logs a
LEFT JOIN users u ON u.id = a.actor_user_id
WHERE (sqlc.narg(actor_user_id)::uuid IS NULL OR a.actor_user_id = sqlc.narg(actor_user_id))
  AND (sqlc.narg(action)::text IS NULL OR a.action = sqlc.narg(action))
  AND (sqlc.narg(target_type)::text IS NULL OR a.target_type = sqlc.narg(target_type))
ORDER BY a.created_at DESC
LIMIT $1 OFFSET $2;
