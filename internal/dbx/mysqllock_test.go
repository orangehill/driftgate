package dbx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestLockStopped(t *testing.T) {
	for err, want := range map[error]bool{
		&mysql.MySQLError{Number: 1213}:                                    true,
		&mysql.MySQLError{Number: 1205}:                                    true,
		fmt.Errorf("t: %w", &mysql.MySQLError{Number: 1213}):               true,
		errors.Join(errors.New("source"), &mysql.MySQLError{Number: 1213}): true,
		&mysql.MySQLError{Number: 1146}:                                    false,
		errors.New("Deadlock found when trying to get lock"):               false,
		context.Canceled: false,
	} {
		if MySQLLockStopped(err) != want {
			t.Errorf("%v: %v", err, !want)
		}
	}
}
