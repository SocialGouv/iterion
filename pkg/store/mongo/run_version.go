package mongo

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// versionRunUpdate makes every partial run write visible to SaveRun's CAS.
// Some transition builders already increment the counter; keep that single
// increment. Full-document SaveRun has its own version-conditioned pipeline.
func versionRunUpdate(update any) any {
	switch u := update.(type) {
	case bson.M:
		inc, _ := u["$inc"].(bson.M)
		if inc == nil {
			inc = bson.M{}
			u["$inc"] = inc
		}
		if _, ok := inc["version"]; !ok {
			inc["version"] = 1
		}
		return u
	case mongo.Pipeline:
		for _, stage := range u {
			for _, op := range stage {
				if op.Key == "$set" {
					if fields, ok := op.Value.(bson.M); ok {
						if _, present := fields["version"]; present {
							return u
						}
					}
				}
			}
		}
		return append(u, bson.D{{Key: "$set", Value: bson.M{"version": bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$version", 0}}, 1}}}}})
	default:
		panic("unsupported run update type")
	}
}
