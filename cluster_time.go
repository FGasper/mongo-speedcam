package main

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func GetClusterTimeFromSession(sess *mongo.Session) (bson.Timestamp, error) {
	ctRaw := sess.ClusterTime()

	rv, err := ctRaw.LookupErr("$clusterTime", "clusterTime")
	if err != nil {
		return bson.Timestamp{}, fmt.Errorf("finding clusterTime in session cluster time document (%v): %w", ctRaw, err)
	}

	t, i := rv.Timestamp()

	return bson.Timestamp{t, i}, nil
}
