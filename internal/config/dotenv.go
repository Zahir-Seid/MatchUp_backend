package config

import "github.com/joho/godotenv"

// LoadDotEnv reads .env from the current working directory when it is present.
// Missing files are ignored: production injects its environment and has no
// .env. Variables already set win, because godotenv only fills unset keys, so
// compose, CI and a developer's shell override the file rather than the other
// way round.
func LoadDotEnv() {
	_ = godotenv.Load()
}
