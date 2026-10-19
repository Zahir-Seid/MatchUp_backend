-- "Notify me when a spot frees up" (PRD FR29). Watching twice is a no-op, not
-- an error: the button's effect is "be watching", and a stale app pressing it
-- again is not a client bug worth reporting.
-- name: CreateGameWatch :exec
INSERT INTO game_watches (user_id, game_id)
VALUES (sqlc.arg(user_id), sqlc.arg(game_id))
ON CONFLICT (user_id, game_id) DO NOTHING;

-- name: DeleteGameWatch :exec
DELETE FROM game_watches
WHERE user_id = sqlc.arg(user_id) AND game_id = sqlc.arg(game_id);

-- name: GetGameWatch :one
SELECT EXISTS (
    SELECT 1 FROM game_watches
    WHERE user_id = sqlc.arg(user_id) AND game_id = sqlc.arg(game_id)
) AS watching;

-- The watchers to tell when a game's seats were just freed (or added). The
-- game side is filtered here rather than by the caller: only a scheduled,
-- future game that now has spots is worth telling anyone about, so a cancelled
-- or started game cannot produce an alert even if its spots were released.
-- name: ListWatchersForAvailableGames :many
SELECT w.id, w.user_id, w.game_id, g.starts_at, f.name AS field_name
FROM game_watches w
JOIN games g ON g.id = w.game_id
JOIN fields f ON f.id = g.field_id
WHERE w.game_id = ANY(sqlc.arg(game_ids)::uuid[])
  AND g.status = 'scheduled'
  AND g.starts_at > now()
  AND g.spots_remaining > 0
ORDER BY w.id;

-- A watch is spent once its alert is enqueued. The delete runs in the same
-- transaction as the enqueue, so an alert and its watch row move together:
-- neither can be lost without the other, and neither can happen twice.
-- name: DeleteGameWatches :exec
DELETE FROM game_watches WHERE id = ANY(sqlc.arg(ids)::uuid[]);
