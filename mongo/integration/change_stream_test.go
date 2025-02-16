// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/internal/assert"
	"go.mongodb.org/mongo-driver/internal/require"
	"go.mongodb.org/mongo-driver/internal/testutil/monitor"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type resumeType int
type streamType int

const (
	minChangeStreamVersion = "3.6.0"
	minPbrtVersion         = "4.0.7"
	minStartAfterVersion   = "4.1.1"

	startAfter resumeType = iota
	resumeAfter
	operationTime

	client streamType = iota
	database
	collection

	errorInterrupted     int32 = 11601
	errorHostUnreachable int32 = 6

	resumableChangeStreamError = "ResumableChangeStreamError"
)

func TestChangeStream_Standalone(t *testing.T) {
	mtOpts := mtest.NewOptions().MinServerVersion(minChangeStreamVersion).CreateClient(false).Topologies(mtest.Single)
	mt := mtest.New(t, mtOpts)
	defer mt.Close()

	mt.Run("no custom standalone error", func(mt *mtest.T) {
		_, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		_, ok := err.(mongo.CommandError)
		assert.True(mt, ok, "expected error type %T, got %T", mongo.CommandError{}, err)
	})
}

func TestChangeStream_ReplicaSet(t *testing.T) {
	mtOpts := mtest.NewOptions().MinServerVersion(minChangeStreamVersion).CreateClient(false).Topologies(mtest.ReplicaSet)
	mt := mtest.New(t, mtOpts)
	defer mt.Close()

	mt.Run("first stage is $changeStream", func(mt *mtest.T) {
		// first stage in the aggregate pipeline must be $changeStream

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)
		started := mt.GetStartedEvent()
		assert.NotNil(mt, started, "expected started event for aggregate, got nil")

		// pipeline is array of documents. first value of first element in array is the first stage document
		firstStage := started.Command.Lookup("pipeline").Array().Index(0).Value().Document()
		elems, _ := firstStage.Elements()
		assert.Equal(mt, 1, len(elems), "expected first stage document to have 1 element, got %v", len(elems))
		firstKey := elems[0].Key()
		want := "$changeStream"
		assert.Equal(mt, want, firstKey, "expected first stage to be %v, got %v", want, firstKey)
	})
	mt.Run("track resume token", func(mt *mtest.T) {
		// ChangeStream must continuously track the last seen resumeToken

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 1)
		assert.True(mt, cs.Next(context.Background()), "expected next to return true, got false")
		assert.NotNil(mt, cs.ResumeToken(), "expected resume token, got nil")
	})
	mt.RunOpts("resume token updated on empty batch", mtest.NewOptions().MinServerVersion("4.0.7"), func(mt *mtest.T) {
		// The resume token is updated when an empty batch is returned using the server's post batch resume token.

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		// cause an event to occur so the resume token is updated
		generateEvents(mt, 1)
		assert.True(mt, cs.Next(context.Background()), "expected next to return true, got false")
		firstToken := cs.ResumeToken()

		// cause an event on a different collection than the one being watched so the server's PBRT is updated
		diffColl := mt.CreateCollection(mtest.Collection{Name: "diffCollUpdatePbrt"}, false)
		_, err = diffColl.InsertOne(context.Background(), bson.D{{"x", 1}})
		assert.Nil(mt, err, "InsertOne error: %v", err)

		// verify that the resume token is updated using the PBRT from an empty batch
		mt.ClearEvents()
		assert.False(mt, cs.TryNext(context.Background()), "unexpected event document: %v", cs.Current)
		assert.Nil(mt, cs.Err(), "change stream error getting new batch: %v", cs.Err())
		newToken := cs.ResumeToken()
		assert.NotEqual(mt, newToken, firstToken, "resume token was not updated after an empty batch was returned")

		evt := mt.GetSucceededEvent()
		assert.Equal(mt, "getMore", evt.CommandName, "expected event for 'getMore', got '%v'", evt.CommandName)
		getMorePbrt := evt.Reply.Lookup("cursor", "postBatchResumeToken").Document()
		assert.Equal(mt, newToken, getMorePbrt, "expected resume token %v, got %v", getMorePbrt, newToken)
	})
	mt.Run("missing resume token", func(mt *mtest.T) {
		// ChangeStream will throw an exception if the server response is missing the resume token

		projectDoc := bson.D{
			{"$project", bson.D{
				{"_id", 0},
			}},
		}
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{projectDoc})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 2)
		assert.False(mt, cs.Next(context.Background()), "expected Next to return false, got true")
		assert.NotNil(mt, cs.Err(), "expected error, got nil")
	})
	mt.RunOpts("resume once", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
		// ChangeStream will automatically resume one time on a resumable error

		// aggregateRes: create change stream with ID 1 and a batch of size 1 so the resume token will be recorded
		// failureGetMoreRes: resumable error
		// killCursorsRes: success
		// resumedAggregateRes: create new change stream with ID 2 and a batch of size 1 so the resume token will be
		// updated
		ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
		aggregateRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch, bson.D{
			{"_id", bson.D{{"first", "resume token"}}},
		})
		failureGetMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorHostUnreachable,
			Name:    "foo",
			Message: "bar",
			Labels:  []string{resumableChangeStreamError},
		})
		killCursorsRes := mtest.CreateSuccessResponse()
		newResumeToken := bson.D{{"second", "resume token"}}
		resumedAggregateRes := mtest.CreateCursorResponse(2, ns, mtest.FirstBatch, bson.D{
			{"_id", newResumeToken},
		})
		mt.AddMockResponses(
			aggregateRes,
			failureGetMoreRes,
			killCursorsRes,
			resumedAggregateRes,
		)

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)
		// Consume the first document
		assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")

		// Clear existing events and expect a resume attempt to happen.
		mt.ClearEvents()
		assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")

		// Next should cause getMore, killCursors, and aggregate to run
		assert.NotNil(mt, mt.GetStartedEvent(), "expected getMore event, got nil")
		assert.NotNil(mt, mt.GetStartedEvent(), "expected killCursors event, got nil")
		aggEvent := mt.GetStartedEvent()
		assert.NotNil(mt, aggEvent, "expected aggregate event, got nil")
		assert.Equal(mt, "aggregate", aggEvent.CommandName, "expected command name 'aggregate', got '%v'", aggEvent.CommandName)

		// Assert that the change stream has updated it's ID and resume token.
		assert.Equal(mt, cs.ID(), int64(2), "expected change stream ID to be 2, got %d", cs.ID())
		newResumeTokenRaw, err := bson.Marshal(newResumeToken)
		assert.Nil(mt, err, "Marshal error: %v", err)
		comparisonErr := compareDocs(mt, newResumeTokenRaw, cs.ResumeToken())
		assert.Nil(mt, comparisonErr, "expected resume token %s, got %s", newResumeTokenRaw, cs.ResumeToken())
	})
	mt.RunOpts("no resume for aggregate errors", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
		// ChangeStream will not attempt to resume on any error encountered while executing an aggregate command

		// aggregate response: empty batch but valid cursor ID
		// getMore response: resumable error
		// killCursors response: success
		// resumed aggregate response: resumable error
		ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
		aggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch)
		getMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorHostUnreachable,
			Name:    "foo",
			Message: "bar",
			Labels:  []string{resumableChangeStreamError},
		})
		killCursorsRes := mtest.CreateSuccessResponse()
		resumedAggRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorHostUnreachable,
			Name:    "foo",
			Message: "bar",
			Labels:  []string{resumableChangeStreamError},
		})
		mt.AddMockResponses(aggRes, getMoreRes, killCursorsRes, resumedAggRes)

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		assert.False(mt, cs.Next(context.Background()), "expected Next to return false, got true")
	})
	mt.RunOpts("server selection before resume", mtest.NewOptions().CreateClient(false), func(mt *mtest.T) {
		// ChangeStream will perform server selection before attempting to resume, using initial readPreference
		mt.Skip("skipping for lack of SDAM monitoring")
	})
	mt.Run("empty batch cursor not closed", func(mt *mtest.T) {
		// Ensure that a cursor returned from an aggregate command with a cursor id and an initial empty batch is not closed

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)
		assert.True(mt, cs.ID() > 0, "expected non-zero ID, got 0")
	})
	mt.RunOpts("ignore errors from killCursors", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
		// The killCursors command sent during the "Resume Process" must not be allowed to throw an exception.

		ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
		aggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch)
		getMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorHostUnreachable,
			Name:    "foo",
			Message: "bar",
			Labels:  []string{"ResumableChangeStreamError"},
		})
		killCursorsRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorInterrupted,
			Name:    "foo",
			Message: "bar",
		})
		changeDoc := bson.D{{"_id", bson.D{{"x", 1}}}}
		resumedAggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch, changeDoc)
		mt.AddMockResponses(aggRes, getMoreRes, killCursorsRes, resumedAggRes)

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
		assert.Nil(mt, cs.Err(), "change stream error: %v", cs.Err())
	})

	startAtOpTimeOpts := mtest.NewOptions().MinServerVersion("4.0").MaxServerVersion("4.0.6")
	mt.RunOpts("include startAtOperationTime", startAtOpTimeOpts, func(mt *mtest.T) {
		// $changeStream stage for ChangeStream against a server >=4.0 and <4.0.7 that has not received any results yet
		// MUST include a startAtOperationTime option when resuming a changestream.

		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 1)
		// kill cursor to force resumable error
		killChangeStreamCursor(mt, cs)

		mt.ClearEvents()
		// change stream should resume once and get new change
		assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
		// Next should cause getMore, killCursors, and aggregate to run
		assert.NotNil(mt, mt.GetStartedEvent(), "expected getMore event, got nil")
		assert.NotNil(mt, mt.GetStartedEvent(), "expected killCursors event, got nil")
		aggEvent := mt.GetStartedEvent()
		assert.NotNil(mt, aggEvent, "expected aggregate event, got nil")
		assert.Equal(mt, "aggregate", aggEvent.CommandName, "expected command name 'aggregate', got '%v'", aggEvent.CommandName)

		// check for startAtOperationTime in pipeline
		csStage := aggEvent.Command.Lookup("pipeline").Array().Index(0).Value().Document() // $changeStream stage
		_, err = csStage.Lookup("$changeStream").Document().LookupErr("startAtOperationTime")
		assert.Nil(mt, err, "startAtOperationTime not included in aggregate command")
	})
	mt.RunOpts("decode does not panic", noClientOpts, func(mt *mtest.T) {
		testCases := []struct {
			name             string
			st               streamType
			minServerVersion string
		}{
			{"client", client, "4.0"},
			{"database", database, "4.0"},
			{"collection", collection, ""},
		}
		for _, tc := range testCases {
			tcOpts := mtest.NewOptions()
			if tc.minServerVersion != "" {
				tcOpts.MinServerVersion(tc.minServerVersion)
			}
			mt.RunOpts(tc.name, tcOpts, func(mt *mtest.T) {
				var cs *mongo.ChangeStream
				var err error
				switch tc.st {
				case client:
					cs, err = mt.Client.Watch(context.Background(), mongo.Pipeline{})
				case database:
					cs, err = mt.DB.Watch(context.Background(), mongo.Pipeline{})
				case collection:
					cs, err = mt.Coll.Watch(context.Background(), mongo.Pipeline{})
				}
				assert.Nil(mt, err, "Watch error: %v", err)
				defer closeStream(cs)

				generateEvents(mt, 1)
				assert.True(mt, cs.Next(context.Background()), "expected Next true, got false")
				var res bson.D
				err = cs.Decode(&res)
				assert.Nil(mt, err, "Decode error: %v", err)
				assert.True(mt, len(res) > 0, "expected non-empty document, got empty")
			})
		}
	})
	mt.Run("maxAwaitTimeMS", func(mt *mtest.T) {
		// maxAwaitTimeMS option should be sent as maxTimeMS on getMore

		opts := options.ChangeStream().SetMaxAwaitTime(100 * time.Millisecond)
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{}, opts)
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		_, err = mt.Coll.InsertOne(context.Background(), bson.D{{"x", 1}})
		assert.Nil(mt, err, "InsertOne error: %v", err)
		mt.ClearEvents()
		assert.True(mt, cs.Next(context.Background()), "expected Next true, got false")

		e := mt.GetStartedEvent()
		assert.NotNil(mt, e, "expected getMore event, got nil")
		_, err = e.Command.LookupErr("maxTimeMS")
		assert.Nil(mt, err, "field maxTimeMS not found in command %v", e.Command)
	})
	mt.RunOpts("resume token", noClientOpts, func(mt *mtest.T) {
		// Prose tests to make assertions on resume tokens for change streams that have not done a getMore yet
		mt.RunOpts("no getMore", noClientOpts, func(mt *mtest.T) {
			pbrtOpts := mtest.NewOptions().MinServerVersion(minPbrtVersion).CreateClient(false)
			mt.RunOpts("with PBRT support", pbrtOpts, func(mt *mtest.T) {
				testCases := []struct {
					name             string
					rt               resumeType
					minServerVersion string
				}{
					{"startAfter", startAfter, minStartAfterVersion},
					{"resumeAfter", resumeAfter, minPbrtVersion},
					{"neither", operationTime, minPbrtVersion},
				}

				for _, tc := range testCases {
					tcOpts := mtest.NewOptions().MinServerVersion(tc.minServerVersion)
					mt.RunOpts(tc.name, tcOpts, func(mt *mtest.T) {
						// create temp stream to get a resume token
						mt.ClearEvents()
						cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
						assert.Nil(mt, err, "Watch error: %v", err)

						// Initial resume token should equal the PBRT in the aggregate command
						pbrt, opTime := getAggregateResponseInfo(mt)
						compareResumeTokens(mt, cs, pbrt)

						numEvents := 5
						generateEvents(mt, numEvents)

						// Iterate over one event to get resume token
						assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
						token := cs.ResumeToken()
						closeStream(cs)

						var numExpectedEvents int
						var initialToken bson.Raw
						var opts *options.ChangeStreamOptions
						switch tc.rt {
						case startAfter:
							numExpectedEvents = numEvents - 1
							initialToken = token
							opts = options.ChangeStream().SetStartAfter(token)
						case resumeAfter:
							numExpectedEvents = numEvents - 1
							initialToken = token
							opts = options.ChangeStream().SetResumeAfter(token)
						case operationTime:
							numExpectedEvents = numEvents
							opts = options.ChangeStream().SetStartAtOperationTime(&opTime)
						}

						// clear slate and create new change stream
						mt.ClearEvents()
						cs, err = mt.Coll.Watch(context.Background(), mongo.Pipeline{}, opts)
						assert.Nil(mt, err, "Watch error: %v", err)
						defer closeStream(cs)

						aggPbrt, _ := getAggregateResponseInfo(mt)
						compareResumeTokens(mt, cs, initialToken)

						for i := 0; i < numExpectedEvents; i++ {
							assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
							// while we're not at the last doc in the batch, the resume token should be the _id of the
							// document
							if i != numExpectedEvents-1 {
								compareResumeTokens(mt, cs, cs.Current.Lookup("_id").Document())
							}
						}
						// at end of batch, the resume token should equal the PBRT of the aggregate
						compareResumeTokens(mt, cs, aggPbrt)
					})
				}
			})

			noPbrtOpts := mtest.NewOptions().MaxServerVersion("4.0.6")
			mt.RunOpts("without PBRT support", noPbrtOpts, func(mt *mtest.T) {
				collName := mt.Coll.Name()
				dbName := mt.Coll.Database().Name()
				cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
				assert.Nil(mt, err, "Watch error: %v", err)
				defer closeStream(cs)

				compareResumeTokens(mt, cs, nil) // should be no resume token because no PBRT
				numEvents := 5
				generateEvents(mt, numEvents)
				// iterate once to get a resume token
				assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
				token := cs.ResumeToken()
				assert.NotNil(mt, token, "expected resume token, got nil")

				testCases := []struct {
					name            string
					opts            *options.ChangeStreamOptions
					iterateStream   bool // whether or not resulting change stream should be iterated
					initialToken    bson.Raw
					numDocsExpected int
				}{
					{"resumeAfter", options.ChangeStream().SetResumeAfter(token), true, token, numEvents - 1},
					{"no options", nil, false, nil, 0},
				}
				for _, tc := range testCases {
					mt.Run(tc.name, func(mt *mtest.T) {
						coll := mt.Client.Database(dbName).Collection(collName)
						cs, err := coll.Watch(context.Background(), mongo.Pipeline{}, tc.opts)
						assert.Nil(mt, err, "Watch error: %v", err)
						defer closeStream(cs)

						compareResumeTokens(mt, cs, tc.initialToken)
						if !tc.iterateStream {
							return
						}

						for i := 0; i < tc.numDocsExpected; i++ {
							assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
							// current resume token should always equal _id of current document
							compareResumeTokens(mt, cs, cs.Current.Lookup("_id").Document())
						}
					})
				}
			})
		})
	})
	mt.RunOpts("try next", noClientOpts, func(mt *mtest.T) {
		mt.Run("existing non-empty batch", func(mt *mtest.T) {
			// If there's already documents in the current batch, TryNext should return true without doing a getMore

			cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.Nil(mt, err, "Watch error: %v", err)
			defer closeStream(cs)
			generateEvents(mt, 5)
			// call Next to make sure a batch is retrieved
			assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false")
			tryNextExistingBatchTest(mt, cs)
		})
		mt.Run("one getMore sent", func(mt *mtest.T) {
			// If the current batch is empty, TryNext should send one getMore and return.

			cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.Nil(mt, err, "Watch error: %v", err)
			defer closeStream(cs)

			mt.ClearEvents()
			// first call to TryNext should return false because first batch was empty so batch cursor returns false
			// without doing a getMore
			// next call to TryNext should attempt a getMore
			for i := 0; i < 2; i++ {
				assert.False(mt, cs.TryNext(context.Background()), "TryNext returned true on iteration %v", i)
			}
			verifyOneGetmoreSent(mt)
		})
		mt.RunOpts("getMore error", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
			// If the getMore attempt errors with a non-resumable error, TryNext returns false

			aggRes := mtest.CreateCursorResponse(50, "foo.bar", mtest.FirstBatch)
			mt.AddMockResponses(aggRes)
			cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.Nil(mt, err, "Watch error: %v", err)
			defer closeStream(cs)
			tryNextGetmoreError(mt, cs)
		})
	})

	customDeploymentOpts := mtest.NewOptions().
		Topologies(mtest.ReplicaSet). // Avoid complexity of sharded fail points.
		MinServerVersion("4.0").      // 4.0 is needed to use replica set fail points.
		CreateClient(false)
	mt.RunOpts("custom deployment", customDeploymentOpts, func(mt *mtest.T) {
		// Tests for the changeStreamDeployment type. These are written as integration tests for ChangeStream rather
		// than unit/integration tests for changeStreamDeployment to ensure that the deployment is correctly wired
		// by ChangeStream when executing an aggregate.

		mt.Run("errors are processed for SDAM on initial aggregate", func(mt *mtest.T) {
			tpm := monitor.NewTestPoolMonitor()
			mt.ResetClient(options.Client().
				SetPoolMonitor(tpm.PoolMonitor).
				SetWriteConcern(mtest.MajorityWc).
				SetReadConcern(mtest.MajorityRc).
				SetRetryReads(false))

			mt.SetFailPoint(mtest.FailPoint{
				ConfigureFailPoint: "failCommand",
				Mode: mtest.FailPointMode{
					Times: 1,
				},
				Data: mtest.FailPointData{
					FailCommands:    []string{"aggregate"},
					CloseConnection: true,
				},
			})

			_, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.NotNil(mt, err, "expected Watch error, got nil")
			assert.True(mt, tpm.IsPoolCleared(), "expected pool to be cleared after non-timeout network error but was not")
		})
		mt.Run("errors are processed for SDAM on getMore", func(mt *mtest.T) {
			tpm := monitor.NewTestPoolMonitor()
			mt.ResetClient(options.Client().
				SetPoolMonitor(tpm.PoolMonitor).
				SetWriteConcern(mtest.MajorityWc).
				SetReadConcern(mtest.MajorityRc).
				SetRetryReads(false))

			mt.SetFailPoint(mtest.FailPoint{
				ConfigureFailPoint: "failCommand",
				Mode: mtest.FailPointMode{
					Times: 1,
				},
				Data: mtest.FailPointData{
					FailCommands:    []string{"getMore"},
					CloseConnection: true,
				},
			})

			cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.Nil(mt, err, "Watch error: %v", err)
			defer closeStream(cs)

			_, err = mt.Coll.InsertOne(context.Background(), bson.D{{"x", 1}})
			assert.Nil(mt, err, "InsertOne error: %v", err)

			assert.True(mt, cs.Next(context.Background()), "expected Next to return true, got false (iteration error %v)",
				cs.Err())
			assert.True(mt, tpm.IsPoolCleared(), "expected pool to be cleared after non-timeout network error but was not")
		})
		mt.Run("errors are processed for SDAM on retried aggregate", func(mt *mtest.T) {
			tpm := monitor.NewTestPoolMonitor()
			mt.ResetClient(options.Client().
				SetPoolMonitor(tpm.PoolMonitor).
				SetRetryReads(true))

			mt.SetFailPoint(mtest.FailPoint{
				ConfigureFailPoint: "failCommand",
				Mode: mtest.FailPointMode{
					Times: 2,
				},
				Data: mtest.FailPointData{
					FailCommands:    []string{"aggregate"},
					CloseConnection: true,
				},
			})

			_, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
			assert.NotNil(mt, err, "expected Watch error, got nil")

			clearedEvents := tpm.Events(func(evt *event.PoolEvent) bool {
				return evt.Type == event.PoolCleared
			})
			assert.Equal(mt, 2, len(clearedEvents), "expected two PoolCleared events, got %d", len(clearedEvents))
		})
	})
	// Setting min server version as 4.0 since v3.6 does not send a "dropEvent"
	mt.RunOpts("call to cursor.Next after cursor closed", mtest.NewOptions().MinServerVersion("4.0"), func(mt *mtest.T) {
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		// Generate insert events
		generateEvents(mt, 5)
		// Call Coll.Drop to generate drop and invalidate event
		err = mt.Coll.Drop(context.Background())
		assert.Nil(mt, err, "Drop error: %v", err)

		// Test that all events were successful
		for i := 0; i < 7; i++ {
			assert.True(mt, cs.Next(context.Background()), "Next returned false at index %d; iteration error: %v", i, cs.Err())
		}

		operationType := cs.Current.Lookup("operationType").StringValue()
		assert.Equal(mt, operationType, "invalidate", "expected invalidate event but returned %q event", operationType)
		// next call to cs.Next should return False since cursor is closed
		assert.False(mt, cs.Next(context.Background()), "expected to return false, but returned true")
	})
	mt.Run("getMore commands are monitored", func(mt *mtest.T) {
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		_, err = mt.Coll.InsertOne(context.Background(), bson.M{"x": 1})
		assert.Nil(mt, err, "InsertOne error: %v", err)

		mt.ClearEvents()
		assert.True(mt, cs.Next(context.Background()), "Next returned false with error %v", cs.Err())
		evt := mt.GetStartedEvent()
		assert.Equal(mt, "getMore", evt.CommandName, "expected command 'getMore', got %q", evt.CommandName)
	})
	mt.Run("killCursors commands are monitored", func(mt *mtest.T) {
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		mt.ClearEvents()
		err = cs.Close(context.Background())
		assert.Nil(mt, err, "Close error: %v", err)
		evt := mt.GetStartedEvent()
		assert.Equal(mt, "killCursors", evt.CommandName, "expected command 'killCursors', got %q", evt.CommandName)
	})
	mt.Run("Custom", func(mt *mtest.T) {
		// Custom options should be a BSON map of option names to Marshalable option values.
		// We use "allowDiskUse" as an example.
		customOpts := bson.M{"allowDiskUse": true}
		opts := options.ChangeStream().SetCustom(customOpts)

		// Create change stream with custom options set.
		mt.ClearEvents()
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{}, opts)
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		// Assert that custom option is passed to the initial aggregate.
		evt := mt.GetStartedEvent()
		assert.Equal(mt, "aggregate", evt.CommandName, "expected command 'aggregate' got, %q", evt.CommandName)

		aduVal, err := evt.Command.LookupErr("allowDiskUse")
		assert.Nil(mt, err, "expected field 'allowDiskUse' in started command not found")
		adu, ok := aduVal.BooleanOK()
		assert.True(mt, ok, "expected field 'allowDiskUse' to be boolean, got %v", aduVal.Type.String())
		assert.True(mt, adu, "expected field 'allowDiskUse' to be true, got false")
	})
	mt.RunOpts("CustomPipeline", mtest.NewOptions().MinServerVersion("4.0"), func(mt *mtest.T) {
		// Custom pipeline options should be a BSON map of option names to Marshalable option values.
		// We use "allChangesForCluster" as an example.
		customPipelineOpts := bson.M{"allChangesForCluster": false}
		opts := options.ChangeStream().SetCustomPipeline(customPipelineOpts)

		// Create change stream with custom pipeline options set.
		mt.ClearEvents()
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{}, opts)
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		// Assert that custom pipeline option is included in the $changeStream stage.
		evt := mt.GetStartedEvent()
		assert.Equal(mt, "aggregate", evt.CommandName, "expected command 'aggregate' got, %q", evt.CommandName)

		acfcVal, err := evt.Command.LookupErr("pipeline", "0", "$changeStream", "allChangesForCluster")
		assert.Nil(mt, err, "expected field 'allChangesForCluster' in $changeStream stage not found")
		acfc, ok := acfcVal.BooleanOK()
		assert.True(mt, ok, "expected field 'allChangesForCluster' to be boolean, got %v", acfcVal.Type.String())
		assert.False(mt, acfc, "expected field 'allChangesForCluster' to be false, got %v", acfc)
	})

	withBSONOpts := mtest.NewOptions().ClientOptions(
		options.Client().SetBSONOptions(&options.BSONOptions{
			UseJSONStructTags: true,
		}))
	mt.RunOpts("with BSONOptions", withBSONOpts, func(mt *mtest.T) {
		cs, err := mt.Coll.Watch(context.Background(), mongo.Pipeline{})
		require.NoError(mt, err, "Watch error")
		defer closeStream(cs)

		type myDocument struct {
			A string `json:"x"`
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mt.Coll.InsertOne(context.Background(), myDocument{A: "foo"})
			require.NoError(mt, err, "InsertOne error")
		}()

		cs.Next(context.Background())

		var got struct {
			FullDocument myDocument `bson:"fullDocument"`
		}
		err = cs.Decode(&got)
		require.NoError(mt, err, "Decode error")

		want := myDocument{
			A: "foo",
		}
		assert.Equal(mt, want, got.FullDocument, "expected and actual Decode results are different")

		wg.Wait()
	})

	splitLargeChangesCollOpts := options.
		CreateCollection().
		SetChangeStreamPreAndPostImages(bson.M{"enabled": true})

	splitLargeChangesOpts := mtOpts.
		MinServerVersion("7.0.0").
		CreateClient(true).
		CollectionCreateOptions(splitLargeChangesCollOpts)

	mt.RunOpts("split large changes", splitLargeChangesOpts, func(mt *mtest.T) {
		type idValue struct {
			ID    int32  `bson:"_id"`
			Value string `bson:"value"`
		}

		doc := idValue{
			ID:    1,
			Value: "q" + strings.Repeat("q", 10*1024*1024),
		}

		// Insert the document
		_, err := mt.Coll.InsertOne(context.Background(), doc)
		require.NoError(t, err, "failed to insert idValue")

		// Watch for change events
		pipeline := mongo.Pipeline{
			{{"$changeStreamSplitLargeEvent", bson.D{}}},
		}

		opts := options.ChangeStream().SetFullDocument(options.Required)

		cs, err := mt.Coll.Watch(context.Background(), pipeline, opts)
		require.NoError(t, err, "failed to watch collection")

		defer closeStream(cs)

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			defer wg.Done()

			filter := bson.D{{"_id", int32(1)}}
			update := bson.D{{"$set", bson.D{{"value", "z" + strings.Repeat("q", 10*1024*1024)}}}}

			_, err := mt.Coll.UpdateOne(context.Background(), filter, update)
			require.NoError(mt, err, "failed to update idValue")
		}()

		nextCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		t.Cleanup(cancel)

		type splitEvent struct {
			Fragment int32 `bson:"fragment"`
			Of       int32 `bson:"of"`
		}

		got := struct {
			SplitEvent splitEvent `bson:"splitEvent"`
		}{}

		cs.Next(nextCtx)

		err = cs.Decode(&got)
		require.NoError(mt, err, "failed to decode first iteration")

		want := splitEvent{
			Fragment: 1,
			Of:       2,
		}

		assert.Equal(mt, want, got.SplitEvent, "expected and actual Decode results are different")

		cs.Next(nextCtx)

		err = cs.Decode(&got)
		require.NoError(mt, err, "failed to decoded second iteration")

		want = splitEvent{
			Fragment: 2,
			Of:       2,
		}

		assert.Equal(mt, want, got.SplitEvent, "expected and actual decode results are different")

		wg.Wait()
	})
}

func closeStream(cs *mongo.ChangeStream) {
	_ = cs.Close(context.Background())
}

func generateEvents(mt *mtest.T, numEvents int) {
	mt.Helper()

	for i := 0; i < numEvents; i++ {
		doc := bson.D{{"x", i}}
		_, err := mt.Coll.InsertOne(context.Background(), doc)
		assert.Nil(mt, err, "InsertOne error on document %v: %v", doc, err)
	}
}

func killChangeStreamCursor(mt *mtest.T, cs *mongo.ChangeStream) {
	mt.Helper()

	db := mt.Coll.Database().Client().Database("admin")
	err := db.RunCommand(context.Background(), bson.D{
		{"killCursors", mt.Coll.Name()},
		{"cursors", bson.A{cs.ID()}},
	}).Err()
	assert.Nil(mt, err, "killCursors error: %v", err)
}

// returns pbrt, operationTime from aggregate command response
func getAggregateResponseInfo(mt *mtest.T) (bson.Raw, primitive.Timestamp) {
	mt.Helper()

	succeeded := mt.GetSucceededEvent()
	assert.NotNil(mt, succeeded, "expected success event for aggregate, got nil")
	assert.Equal(mt, "aggregate", succeeded.CommandName, "expected command name 'aggregate', got '%v'", succeeded.CommandName)

	pbrt := succeeded.Reply.Lookup("cursor", "postBatchResumeToken").Document()
	optimeT, optimeI := succeeded.Reply.Lookup("operationTime").Timestamp()
	return pbrt, primitive.Timestamp{T: optimeT, I: optimeI}
}

func compareResumeTokens(mt *mtest.T, cs *mongo.ChangeStream, expected bson.Raw) {
	mt.Helper()
	assert.Equal(mt, expected, cs.ResumeToken(), "expected resume token %v, got %v", expected, cs.ResumeToken())
}
