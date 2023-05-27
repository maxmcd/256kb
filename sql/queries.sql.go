// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.18.0
// source: queries.sql

package db

import (
	"context"
)

const newBuild = `-- name: NewBuild :one
INSERT INTO build (command)
VALUES (?)
RETURNING id, created_at, completed_at, exit_code, command
`

func (q *Queries) NewBuild(ctx context.Context, command string) (Build, error) {
	row := q.queryRow(ctx, q.newBuildStmt, newBuild, command)
	var i Build
	err := row.Scan(
		&i.ID,
		&i.CreatedAt,
		&i.CompletedAt,
		&i.ExitCode,
		&i.Command,
	)
	return i, err
}

const selectBuild = `-- name: SelectBuild :one
select id, created_at, completed_at, exit_code, command
from build
where id = ?
`

func (q *Queries) SelectBuild(ctx context.Context, id int64) (Build, error) {
	row := q.queryRow(ctx, q.selectBuildStmt, selectBuild, id)
	var i Build
	err := row.Scan(
		&i.ID,
		&i.CreatedAt,
		&i.CompletedAt,
		&i.ExitCode,
		&i.Command,
	)
	return i, err
}