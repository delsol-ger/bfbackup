package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"bfbackup/internal/model"
)

// Drive is a USB drive initialized as a backup target on the controller.
// Its targets have node_id model.DriveNodeID(UUID).
type Drive struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	LastMount string `json:"last_mount"`
	FSType    string `json:"fstype"`
	Total     int64  `json:"total"`
	Free      int64  `json:"free"`
	// OnConnect backs up every target on the drive when it is plugged in.
	OnConnect bool `json:"on_connect"`
	// VerifyAfter runs a quick verify after each of those backups.
	VerifyAfter bool `json:"verify_after"`
	// OverdueDays: notify when the drive was not connected for that many
	// days (0 = never).
	OverdueDays     int   `json:"overdue_days"`
	OverdueNotified int64 `json:"overdue_notified"`
}

// MaxOverdueDays is the longest overdue alert delay.
const MaxOverdueDays = 3650

// Validate checks and normalizes the editable fields.
func (d *Drive) Validate() error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" || len(d.Name) > 60 {
		return errors.New("drive name is required (max 60 characters)")
	}
	if d.OverdueDays < 0 || d.OverdueDays > MaxOverdueDays {
		return fmt.Errorf("overdue days must be between 0 and %d", MaxOverdueDays)
	}
	return nil
}

const driveCols = `uuid, name, created_at, last_seen, last_mount, fstype, total, free, on_connect, verify_after, overdue_days, overdue_notified`

func scanDrive(r interface{ Scan(...any) error }) (Drive, error) {
	var d Drive
	err := r.Scan(&d.UUID, &d.Name, &d.CreatedAt, &d.LastSeen, &d.LastMount, &d.FSType, &d.Total, &d.Free,
		&d.OnConnect, &d.VerifyAfter, &d.OverdueDays, &d.OverdueNotified)
	return d, err
}

// CreateDrive stores a new drive.
func (s *Store) CreateDrive(ctx context.Context, d *Drive) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = s.now().Unix()
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO drives("+driveCols+") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		d.UUID, d.Name, d.CreatedAt, d.LastSeen, d.LastMount, d.FSType, d.Total, d.Free,
		d.OnConnect, d.VerifyAfter, d.OverdueDays, d.OverdueNotified)
	return uniqueErr(err, "a drive with this name or ID already exists")
}

// ListDrives returns all drives by name.
func (s *Store) ListDrives(ctx context.Context) ([]Drive, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+driveCols+" FROM drives ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Drive{}
	for rows.Next() {
		d, err := scanDrive(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDrive returns one drive.
func (s *Store) GetDrive(ctx context.Context, uuid string) (Drive, error) {
	d, err := scanDrive(s.db.QueryRowContext(ctx, "SELECT "+driveCols+" FROM drives WHERE uuid = ?", uuid))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// UpdateDrive changes name and settings.
func (s *Store) UpdateDrive(ctx context.Context, d *Drive) error {
	if err := d.Validate(); err != nil {
		return err
	}
	err := s.execOne(ctx, "UPDATE drives SET name = ?, on_connect = ?, verify_after = ?, overdue_days = ? WHERE uuid = ?",
		d.Name, d.OnConnect, d.VerifyAfter, d.OverdueDays, d.UUID)
	return uniqueErr(err, "a drive with this name already exists")
}

// SeenDrive records that a drive is connected now, where and how full.
func (s *Store) SeenDrive(ctx context.Context, uuid, mount, fstype string, total, free int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE drives SET last_seen = ?, last_mount = ?, fstype = ?, total = ?, free = ?, overdue_notified = 0 WHERE uuid = ?",
		s.now().Unix(), mount, fstype, total, free, uuid)
	return err
}

// SetDriveOverdueNotified records when the overdue alert was sent.
func (s *Store) SetDriveOverdueNotified(ctx context.Context, uuid string, at int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE drives SET overdue_notified = ? WHERE uuid = ?", at, uuid)
	return err
}

// DeleteDrive removes a drive, its targets and their index (not the files).
func (s *Store) DeleteDrive(ctx context.Context, uuid string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM targets WHERE node_id = ?", model.DriveNodeID(uuid)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM drives WHERE uuid = ?", uuid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
