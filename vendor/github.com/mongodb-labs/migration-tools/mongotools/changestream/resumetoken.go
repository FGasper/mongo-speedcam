package changestream

import (
	"encoding/hex"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

const (
	// Type identifier for a resume token’s timestamp
	// (cf. https://github.com/mongodb-js/mongodb-resumetoken-decoder/blob/2d64962d194a5b99bb28ad1da6e7f1e26f6db0b7/src/keystringdecoder.ts#L20)
	rtTimeStampType uint8 = 130
)

func getTsFromStringResumeToken(dataString string) (bson.Timestamp, error) {
	keyStringBinData, decodeErr := hex.DecodeString(dataString)
	if decodeErr != nil {
		return bson.Timestamp{}, fmt.Errorf(
			"failed to decode to hex resume token: %w",
			decodeErr,
		)
	}

	typeIdent := keyStringBinData[0]
	if typeIdent != rtTimeStampType {
		return bson.Timestamp{}, fmt.Errorf("wrong type identifier: got %v, want %v", typeIdent, rtTimeStampType)
	}

	t, i, _, ok := bsoncore.ReadTimestamp(keyStringBinData[1:])
	if !ok {
		return bson.Timestamp{}, fmt.Errorf("failed to read timestamp from resume token")
	}

	return bson.Timestamp{t, i}, nil
}
