package mongo

import (
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	"testing"
)

func TestUsageOriginMongo(t *testing.T) {
	storetest.RunUsageOriginConformance(t, func(t *testing.T) store.RunStore { return correctionMongoStore(t) })
}
