package store_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

func TestMain(m *testing.M) { os.Exit(hometest.Isolate(m.Run)) }
