# MatchUp Backend

Go backend for MatchUp's football-field booking platform.

The repository builds four processes:

- `api` - player-facing HTTP API
- `admin` - isolated administration API
- `worker` - payment verification, hold expiry, and notification delivery
- `cli` - operator tool for account bootstrap and recovery

Run `make help` for the available development, test, and database commands.

## First admin

There is no admin signup route. Create the first admin with the CLI:

```sh
go run ./cmd/cli createsuperuser
# or, in the container image:
docker run -it --env-file .env matchup /app/cli createsuperuser
```

The command prompts for email, full name, phone, and a hidden password, then
creates an active, email-verified `admin` account. Admin passwords require at
least 10 characters (players need 8). Phones may be entered as `09...`, `07...`,
`+2519...`, or `+2517...` and are stored canonically as `+251...`. The command
revokes nothing and signs nobody in. Sign in afterwards at
`POST /admin/v1/auth/login` on the admin port.

Other commands:

```sh
go run ./cmd/cli reset-password --email admin@example.com
go run ./cmd/cli promote --email user@example.com
go run ./cmd/cli demote  --email user@example.com
```

`reset-password` only resets admin accounts and revokes every refresh token and
outstanding reset link for the account; players use the in-app reset flow.
`demote` refuses to remove the last active admin. The email can be omitted and
will be prompted for. In a terminal, an invalid email or unknown account is
re-asked instead of ending the command, and confirmations accept `y`/`yes` or
`n`/`no`. Role changes require confirmation; when stdin is not a terminal, pass
`--yes`. For automation, pass `--password-stdin` and pipe one password line -
there is deliberately no password argument, because arguments are visible in
shell history and process listings.

Every command uses parameterised SQL through the auth service. The CLI never
starts a process and never interprets its input as a command.
