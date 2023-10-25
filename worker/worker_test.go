// Copyright 2020 gorse Project Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/bits-and-blooms/bitset"
	"github.com/scylladb/go-set/strset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	"github.com/thoas/go-funk"
	"github.com/zhenghaoz/gorse/base"
	"github.com/zhenghaoz/gorse/base/parallel"
	"github.com/zhenghaoz/gorse/config"
	"github.com/zhenghaoz/gorse/model"
	"github.com/zhenghaoz/gorse/model/click"
	"github.com/zhenghaoz/gorse/model/ranking"
	"github.com/zhenghaoz/gorse/protocol"
	"github.com/zhenghaoz/gorse/storage/cache"
	"github.com/zhenghaoz/gorse/storage/data"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type WorkerTestSuite struct {
	suite.Suite
	Worker
	dataStoreServer  *miniredis.Miniredis
	cacheStoreServer *miniredis.Miniredis
}

func (suite *WorkerTestSuite) SetupSuite() {
	// create mock redis server
	var err error
	suite.dataStoreServer, err = miniredis.Run()
	suite.NoError(err)
	suite.cacheStoreServer, err = miniredis.Run()
	suite.NoError(err)
	// open database
	suite.Settings = config.NewSettings()
	suite.DataClient, err = data.Open("redis://"+suite.dataStoreServer.Addr(), "")
	suite.NoError(err)
	suite.CacheClient, err = cache.Open("redis://"+suite.cacheStoreServer.Addr(), "")
	suite.NoError(err)
}

func (suite *WorkerTestSuite) TearDownSuite() {
	err := suite.DataClient.Close()
	suite.NoError(err)
	err = suite.CacheClient.Close()
	suite.NoError(err)
	suite.dataStoreServer.Close()
	suite.cacheStoreServer.Close()
}

func (suite *WorkerTestSuite) SetupTest() {
	err := suite.DataClient.Purge()
	suite.NoError(err)
	err = suite.CacheClient.Purge()
	suite.NoError(err)
	// configuration
	suite.Config = config.GetDefaultConfig()
	suite.jobs = 1
	// reset random generator
	suite.randGenerator = rand.New(rand.NewSource(0))
}

func (suite *WorkerTestSuite) TestPullUsers() {
	ctx := context.Background()
	// create user index
	err := suite.DataClient.BatchInsertUsers(ctx, []data.User{
		{UserId: "1"},
		{UserId: "2"},
		{UserId: "3"},
		{UserId: "4"},
		{UserId: "5"},
		{UserId: "6"},
		{UserId: "7"},
		{UserId: "8"},
	})
	suite.NoError(err)
	// create nodes
	nodes := []string{"a", "b", "c"}

	users, err := suite.pullUsers(nodes, "b")
	suite.NoError(err)
	suite.Equal([]data.User{{UserId: "1"}, {UserId: "3"}, {UserId: "6"}}, users)

	_, err = suite.pullUsers(nodes, "d")
	suite.Error(err)
}

func (suite *WorkerTestSuite) TestCheckRecommendCacheTimeout() {
	ctx := context.Background()

	// empty cache
	suite.True(suite.checkRecommendCacheTimeout(ctx, "0", nil))
	err := suite.CacheClient.SetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), []cache.Scored{{"0", 0}})
	suite.NoError(err)

	// digest mismatch
	suite.True(suite.checkRecommendCacheTimeout(ctx, "0", nil))
	err = suite.CacheClient.Set(ctx, cache.String(cache.Key(cache.OfflineRecommendDigest, "0"), suite.Config.OfflineRecommendDigest()))
	suite.NoError(err)

	err = suite.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastModifyUserTime, "0"), time.Now().Add(-time.Hour)))
	suite.NoError(err)
	suite.True(suite.checkRecommendCacheTimeout(ctx, "0", nil))
	err = suite.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastUpdateUserRecommendTime, "0"), time.Now().Add(-time.Hour*100)))
	suite.NoError(err)
	suite.True(suite.checkRecommendCacheTimeout(ctx, "0", nil))
	err = suite.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastUpdateUserRecommendTime, "0"), time.Now().Add(time.Hour*100)))
	suite.NoError(err)
	suite.False(suite.checkRecommendCacheTimeout(ctx, "0", nil))
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), nil)
	suite.NoError(err)
	suite.True(suite.checkRecommendCacheTimeout(ctx, "0", nil))
}

type mockMatrixFactorizationForRecommend struct {
	ranking.BaseMatrixFactorization
}

func (m *mockMatrixFactorizationForRecommend) Complexity() int {
	panic("implement me")
}

func newMockMatrixFactorizationForRecommend(numUsers, numItems int) *mockMatrixFactorizationForRecommend {
	m := new(mockMatrixFactorizationForRecommend)
	m.UserIndex = base.NewMapIndex()
	m.ItemIndex = base.NewMapIndex()
	for i := 0; i < numUsers; i++ {
		m.UserIndex.Add(strconv.Itoa(i))
	}
	for i := 0; i < numItems; i++ {
		m.ItemIndex.Add(strconv.Itoa(i))
	}
	m.UserPredictable = bitset.New(uint(numUsers)).Complement()
	m.ItemPredictable = bitset.New(uint(numItems)).Complement()
	return m
}

func (m *mockMatrixFactorizationForRecommend) GetUserFactor(_ int32) []float32 {
	return []float32{1}
}

func (m *mockMatrixFactorizationForRecommend) GetItemFactor(itemId int32) []float32 {
	return []float32{float32(itemId)}
}

func (m *mockMatrixFactorizationForRecommend) Invalid() bool {
	return false
}

func (m *mockMatrixFactorizationForRecommend) Fit(_, _ *ranking.DataSet, _ *ranking.FitConfig) ranking.Score {
	panic("implement me")
}

func (m *mockMatrixFactorizationForRecommend) Predict(_, itemId string) float32 {
	itemIndex, err := strconv.Atoi(itemId)
	if err != nil {
		panic(err)
	}
	return float32(itemIndex)
}

func (m *mockMatrixFactorizationForRecommend) InternalPredict(_, itemId int32) float32 {
	return float32(itemId)
}

func (m *mockMatrixFactorizationForRecommend) Clear() {
	// do nothing
}

func (m *mockMatrixFactorizationForRecommend) GetParamsGrid(_ bool) model.ParamsGrid {
	panic("don't call me")
}

func (suite *WorkerTestSuite) TestRecommendMatrixFactorizationBruteForce() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = true
	suite.Config.Recommend.Collaborative.EnableIndex = false
	// insert feedbacks
	now := time.Now()
	err := suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "9"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "8"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "7"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "6"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "5"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "4"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "3"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "2"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "1"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "0"}, Timestamp: now.Add(time.Hour)},
	}, true, true, true)
	suite.NoError(err)

	// insert hidden items and categorized items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "10", IsHidden: true},
		{ItemId: "11", IsHidden: true},
		{ItemId: "3", Categories: []string{"*"}},
		{ItemId: "1", Categories: []string{"*"}},
	})
	suite.NoError(err)

	// create mock model
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 12)
	suite.Recommend([]data.User{{UserId: "0"}})

	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{
		{"3", 3},
		{"2", 2},
		{"1", 1},
		{"0", 0},
	}, recommends)
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{
		{"3", 3},
		{"1", 1},
	}, recommends)

	readCache, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.IgnoreItems, "0"), 0, -1)
	read := cache.RemoveScores(readCache)
	suite.NoError(err)
	suite.ElementsMatch([]string{"0", "1", "2", "3"}, read)
	for _, v := range readCache {
		suite.Greater(v.Score, float64(time.Now().Unix()))
	}
}

func (suite *WorkerTestSuite) TestRecommendMatrixFactorizationHNSW() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = true
	suite.Config.Recommend.Collaborative.EnableIndex = true
	// insert feedbacks
	now := time.Now()
	err := suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "9"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "8"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "7"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "6"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "5"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "4"}, Timestamp: now.Add(-time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "3"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "2"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "1"}, Timestamp: now.Add(time.Hour)},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "click", UserId: "0", ItemId: "0"}, Timestamp: now.Add(time.Hour)},
	}, true, true, true)
	suite.NoError(err)

	// insert hidden items and categorized items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "10", IsHidden: true},
		{ItemId: "11", IsHidden: true},
		{ItemId: "3", Categories: []string{"*"}},
		{ItemId: "1", Categories: []string{"*"}},
	})
	suite.NoError(err)

	// create mock model
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 12)
	suite.Recommend([]data.User{{UserId: "0"}})

	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{
		{"3", 3},
		{"2", 2},
		{"1", 1},
		{"0", 0},
	}, recommends)

	readCache, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.IgnoreItems, "0"), 0, -1)
	read := cache.RemoveScores(readCache)
	suite.NoError(err)
	suite.ElementsMatch([]string{"0", "1", "2", "3"}, read)
	for _, v := range readCache {
		suite.Greater(v.Score, float64(time.Now().Unix()))
	}
}

func (suite *WorkerTestSuite) TestRecommendItemBased() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnableItemBasedRecommend = true
	// insert feedback
	err := suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "21"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "22"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "23"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "0", ItemId: "24"}},
	}, true, true, true)
	suite.NoError(err)

	// insert similar items
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "21"), []cache.Scored{
		{"22", 100000},
		{"25", 1000000},
		{"29", 1},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "22"), []cache.Scored{
		{"23", 100000},
		{"25", 1000000},
		{"28", 1},
		{"29", 1},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "23"), []cache.Scored{
		{"24", 100000},
		{"25", 1000000},
		{"27", 1},
		{"28", 1},
		{"29", 1},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "24"), []cache.Scored{
		{"21", 100000},
		{"25", 1000000},
		{"26", 1},
		{"27", 1},
		{"28", 1},
		{"29", 1},
	})
	suite.NoError(err)

	// insert similar items in category
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "21", "*"), []cache.Scored{
		{"22", 100000},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "22", "*"), []cache.Scored{
		{"28", 1},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "23", "*"), []cache.Scored{
		{"24", 100000},
		{"28", 1},
	})
	suite.NoError(err)
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.ItemNeighbors, "24", "*"), []cache.Scored{
		{"26", 1},
		{"28", 1},
	})
	suite.NoError(err)

	// insert items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "21"}, {ItemId: "22"}, {ItemId: "23"}, {ItemId: "24"},
		{ItemId: "25"}, {ItemId: "26"}, {ItemId: "27"}, {ItemId: "28"}, {ItemId: "29"}})
	suite.NoError(err)
	// insert hidden items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "25", IsHidden: true}})
	suite.NoError(err)
	// insert categorized items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "26", Categories: []string{"*"}}, {ItemId: "28", Categories: []string{"*"}}})
	suite.NoError(err)
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 10)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"29", 29}, {"28", 28}, {"27", 27}}, recommends)
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"28", 28}, {"26", 26}}, recommends)
}

func (suite *WorkerTestSuite) TestRecommendUserBased() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnableUserBasedRecommend = true
	// insert similar users
	err := suite.CacheClient.SetSorted(ctx, cache.Key(cache.UserNeighbors, "0"), []cache.Scored{
		{"1", 2},
		{"2", 1.5},
		{"3", 1},
	})
	suite.NoError(err)
	// insert feedback
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "1", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "1", ItemId: "11"}},
	}, true, true, true)
	suite.NoError(err)
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "2", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "2", ItemId: "12"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "2", ItemId: "48"}},
	}, true, true, true)
	suite.NoError(err)
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "3", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "3", ItemId: "13"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "a", UserId: "3", ItemId: "48"}},
	}, true, true, true)
	suite.NoError(err)
	// insert hidden items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "10", IsHidden: true}})
	suite.NoError(err)
	// insert categorized items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "12", Categories: []string{"*"}},
		{ItemId: "48", Categories: []string{"*"}},
	})
	suite.NoError(err)
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 10)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"48", 48}, {"13", 13}, {"12", 12}}, recommends)
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"48", 48}, {"12", 12}}, recommends)
}

func (suite *WorkerTestSuite) TestRecommendPopular() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnablePopularRecommend = true
	// insert popular items
	err := suite.CacheClient.SetSorted(ctx, cache.PopularItems, []cache.Scored{{"11", 11}, {"10", 10}, {"9", 9}, {"8", 8}})
	suite.NoError(err)
	// insert popular items with category *
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.PopularItems, "*"), []cache.Scored{{"20", 20}, {"19", 19}, {"18", 18}})
	suite.NoError(err)
	// insert items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "11"}, {ItemId: "10"}, {ItemId: "9"}, {ItemId: "8"},
		{ItemId: "20", Categories: []string{"*"}},
		{ItemId: "19", Categories: []string{"*"}},
		{ItemId: "18", Categories: []string{"*"}},
	})
	suite.NoError(err)
	// insert hidden items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "11", IsHidden: true}})
	suite.NoError(err)
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 10)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 10}, {"9", 9}, {"8", 8}}, recommends)
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"20", 20}, {"19", 19}, {"18", 18}}, recommends)
}

func (suite *WorkerTestSuite) TestRecommendLatest() {
	// create mock worker
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnableLatestRecommend = true
	// insert latest items
	err := suite.CacheClient.SetSorted(ctx, cache.LatestItems, []cache.Scored{{"11", 11}, {"10", 10}, {"9", 9}, {"8", 8}})
	suite.NoError(err)
	// insert the latest items with category *
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.LatestItems, "*"), []cache.Scored{{"20", 10}, {"19", 9}, {"18", 8}})
	suite.NoError(err)
	// insert items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "11"}, {ItemId: "10"}, {ItemId: "9"}, {ItemId: "8"},
		{ItemId: "20", Categories: []string{"*"}},
		{ItemId: "19", Categories: []string{"*"}},
		{ItemId: "18", Categories: []string{"*"}},
	})
	suite.NoError(err)
	// insert hidden items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "11", IsHidden: true}})
	suite.NoError(err)
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 10)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 10}, {"9", 9}, {"8", 8}}, recommends)
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, -1)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"20", 20}, {"19", 19}, {"18", 18}}, recommends)
}

func (suite *WorkerTestSuite) TestRecommendColdStart() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.EnableColRecommend = true
	suite.Config.Recommend.Offline.EnableLatestRecommend = true
	// insert latest items
	err := suite.CacheClient.SetSorted(ctx, cache.LatestItems, []cache.Scored{{"11", 11}, {"10", 10}, {"9", 9}, {"8", 8}})
	suite.NoError(err)
	// insert the latest items with category *
	err = suite.CacheClient.SetSorted(ctx, cache.Key(cache.LatestItems, "*"), []cache.Scored{{"20", 10}, {"19", 9}, {"18", 8}})
	suite.NoError(err)
	// insert items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "11"}, {ItemId: "10"}, {ItemId: "9"}, {ItemId: "8"},
		{ItemId: "20", Categories: []string{"*"}},
		{ItemId: "19", Categories: []string{"*"}},
		{ItemId: "18", Categories: []string{"*"}},
	})
	suite.NoError(err)
	// insert hidden items
	err = suite.DataClient.BatchInsertItems(ctx, []data.Item{{ItemId: "11", IsHidden: true}})
	suite.NoError(err)

	// ranking model not exist
	m := newMockMatrixFactorizationForRecommend(10, 100)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, -1)
	suite.NoError(err)
	suite.Equal([]string{"10", "9", "8"}, cache.RemoveScores(recommends))
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0", "*"), 0, -1)
	suite.NoError(err)
	suite.Equal([]string{"20", "19", "18"}, cache.RemoveScores(recommends))

	// user not predictable
	suite.RankingModel = m
	suite.Recommend([]data.User{{UserId: "100"}})
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "100"), 0, -1)
	suite.NoError(err)
	suite.Equal([]string{"10", "9", "8"}, cache.RemoveScores(recommends))
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "100", "*"), 0, -1)
	suite.NoError(err)
	suite.Equal([]string{"20", "19", "18"}, cache.RemoveScores(recommends))
}

func (suite *WorkerTestSuite) TestMergeAndShuffle() {
	scores := suite.mergeAndShuffle([][]string{{"1", "2", "3"}, {"1", "3", "5"}})
	suite.ElementsMatch([]string{"1", "2", "3", "5"}, cache.RemoveScores(scores))
}

func (suite *WorkerTestSuite) TestExploreRecommend() {
	ctx := context.Background()
	suite.Config.Recommend.Offline.ExploreRecommend = map[string]float64{"popular": 0.3, "latest": 0.3}
	// insert popular items
	err := suite.CacheClient.SetSorted(ctx, cache.PopularItems, []cache.Scored{{"popular", 0}})
	suite.NoError(err)
	// insert latest items
	err = suite.CacheClient.SetSorted(ctx, cache.LatestItems, []cache.Scored{{"latest", 0}})
	suite.NoError(err)

	recommend, err := suite.exploreRecommend(cache.CreateScoredItems(
		funk.ReverseStrings([]string{"1", "2", "3", "4", "5", "6", "7", "8"}),
		funk.ReverseFloat64([]float64{1, 2, 3, 4, 5, 6, 7, 8})), strset.New(), "")
	suite.NoError(err)
	items := cache.RemoveScores(recommend)
	suite.Contains(items, "latest")
	suite.Contains(items, "popular")
	items = funk.FilterString(items, func(item string) bool {
		return item != "latest" && item != "popular"
	})
	suite.IsDecreasing(items)
	scores := cache.GetScores(recommend)
	suite.IsDecreasing(scores)
	suite.Equal(8, len(recommend))
}

func marshal(t *testing.T, v interface{}) string {
	s, err := json.Marshal(v)
	assert.NoError(t, err)
	return string(s)
}

func newRankingDataset() (*ranking.DataSet, *ranking.DataSet) {
	dataset := &ranking.DataSet{
		UserIndex: base.NewMapIndex(),
		ItemIndex: base.NewMapIndex(),
	}
	return dataset, dataset
}

func newClickDataset() (*click.Dataset, *click.Dataset) {
	dataset := &click.Dataset{
		Index: click.NewUnifiedMapIndexBuilder().Build(),
	}
	return dataset, dataset
}

type mockMaster struct {
	protocol.UnimplementedMasterServer
	addr         chan string
	grpcServer   *grpc.Server
	cacheStore   *miniredis.Miniredis
	dataStore    *miniredis.Miniredis
	meta         *protocol.Meta
	rankingModel []byte
	clickModel   []byte
	userIndex    []byte
}

func newMockMaster(t *testing.T) *mockMaster {
	cacheStore, err := miniredis.Run()
	assert.NoError(t, err)
	dataStore, err := miniredis.Run()
	assert.NoError(t, err)
	cfg := config.GetDefaultConfig()
	cfg.Database.DataStore = "redis://" + dataStore.Addr()
	cfg.Database.CacheStore = "redis://" + cacheStore.Addr()

	// create click model
	train, test := newClickDataset()
	fm := click.NewFM(click.FMClassification, model.Params{model.NEpochs: 0})
	fm.Fit(train, test, nil)
	clickModelBuffer := bytes.NewBuffer(nil)
	err = click.MarshalModel(clickModelBuffer, fm)
	assert.NoError(t, err)

	// create ranking model
	trainSet, testSet := newRankingDataset()
	bpr := ranking.NewBPR(model.Params{model.NEpochs: 0})
	bpr.Fit(trainSet, testSet, nil)
	rankingModelBuffer := bytes.NewBuffer(nil)
	err = ranking.MarshalModel(rankingModelBuffer, bpr)
	assert.NoError(t, err)

	// create user index
	userIndexBuffer := bytes.NewBuffer(nil)
	err = base.MarshalIndex(userIndexBuffer, base.NewMapIndex())
	assert.NoError(t, err)

	return &mockMaster{
		addr: make(chan string),
		meta: &protocol.Meta{
			Config:              marshal(t, cfg),
			ClickModelVersion:   1,
			RankingModelVersion: 2,
		},
		cacheStore:   cacheStore,
		dataStore:    dataStore,
		userIndex:    userIndexBuffer.Bytes(),
		clickModel:   clickModelBuffer.Bytes(),
		rankingModel: rankingModelBuffer.Bytes(),
	}
}

func (m *mockMaster) GetMeta(_ context.Context, _ *protocol.NodeInfo) (*protocol.Meta, error) {
	return m.meta, nil
}

func (m *mockMaster) GetRankingModel(_ *protocol.VersionInfo, sender protocol.Master_GetRankingModelServer) error {
	return sender.Send(&protocol.Fragment{Data: m.rankingModel})
}

func (m *mockMaster) GetClickModel(_ *protocol.VersionInfo, sender protocol.Master_GetClickModelServer) error {
	return sender.Send(&protocol.Fragment{Data: m.clickModel})
}

func (m *mockMaster) Start(t *testing.T) {
	listen, err := net.Listen("tcp", "localhost:0")
	assert.NoError(t, err)
	m.addr <- listen.Addr().String()
	var opts []grpc.ServerOption
	m.grpcServer = grpc.NewServer(opts...)
	protocol.RegisterMasterServer(m.grpcServer, m)
	err = m.grpcServer.Serve(listen)
	assert.NoError(t, err)
}

func (m *mockMaster) Stop() {
	m.grpcServer.Stop()
	m.dataStore.Close()
	m.cacheStore.Close()
}

func TestWorker_Sync(t *testing.T) {
	master := newMockMaster(t)
	go master.Start(t)
	address := <-master.addr
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	serv := &Worker{
		Settings:     config.NewSettings(),
		testMode:     true,
		masterClient: protocol.NewMasterClient(conn),
		syncedChan:   parallel.NewConditionChannel(),
		ticker:       time.NewTicker(time.Minute),
	}

	// This clause is used to test race condition.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				p, _ := serv.Config.Recommend.Offline.GetExploreRecommend("popular")
				assert.Zero(t, p)
			}
		}
	}()

	serv.Sync()
	assert.Equal(t, "redis://"+master.dataStore.Addr(), serv.dataPath)
	assert.Equal(t, "redis://"+master.cacheStore.Addr(), serv.cachePath)
	assert.Equal(t, int64(1), serv.latestClickModelVersion)
	assert.Equal(t, int64(2), serv.latestRankingModelVersion)
	assert.Zero(t, serv.ClickModelVersion)
	assert.Zero(t, serv.RankingModelVersion)
	serv.Pull()
	assert.Equal(t, int64(1), serv.ClickModelVersion)
	assert.Equal(t, int64(2), serv.RankingModelVersion)
	master.Stop()
	done <- struct{}{}
}

func TestWorker_SyncRecommend(t *testing.T) {
	cfg := config.GetDefaultConfig()
	cfg.Recommend.Offline.ExploreRecommend = map[string]float64{"popular": 0.5}
	master := newMockMaster(t)
	master.meta.Config = marshal(t, cfg)
	go master.Start(t)
	address := <-master.addr
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NoError(t, err)
	worker := &Worker{
		Settings:     config.NewSettings(),
		jobs:         1,
		testMode:     true,
		masterClient: protocol.NewMasterClient(conn),
		syncedChan:   parallel.NewConditionChannel(),
		ticker:       time.NewTicker(time.Minute),
	}
	worker.Sync()

	stopSync := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopSync:
				return
			default:
				worker.Sync()
			}
		}
	}()

	stopRecommend := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopRecommend:
				return
			default:
				worker.Settings.Config.OfflineRecommendDigest()
			}
		}
	}()

	time.Sleep(time.Second)
	stopSync <- struct{}{}
	stopRecommend <- struct{}{}
	master.Stop()
}

type mockFactorizationMachine struct {
	click.BaseFactorizationMachine
}

func (m mockFactorizationMachine) Complexity() int {
	panic("implement me")
}

func (m mockFactorizationMachine) Bytes() int {
	panic("implement me")
}

func (m mockFactorizationMachine) GetParamsGrid(_ bool) model.ParamsGrid {
	panic("implement me")
}

func (m mockFactorizationMachine) Clear() {
	panic("implement me")
}

func (m mockFactorizationMachine) Invalid() bool {
	return false
}

func (m mockFactorizationMachine) Predict(_, itemId string, _, _ []string) float32 {
	score, err := strconv.Atoi(itemId)
	if err != nil {
		panic(err)
	}
	return float32(score)
}

func (m mockFactorizationMachine) InternalPredict(_ []int32, _ []float32) float32 {
	panic("implement me")
}

func (m mockFactorizationMachine) Fit(_, _ *click.Dataset, _ *click.FitConfig) click.Score {
	panic("implement me")
}

func (m mockFactorizationMachine) Marshal(_ io.Writer) error {
	panic("implement me")
}

func (suite *WorkerTestSuite) TestRankByCollaborativeFiltering() {
	ctx := context.Background()
	// insert a user
	err := suite.DataClient.BatchInsertUsers(ctx, []data.User{{UserId: "1"}})
	suite.NoError(err)
	// insert items
	itemCache := make(map[string]data.Item)
	for i := 1; i <= 5; i++ {
		itemCache[strconv.Itoa(i)] = data.Item{ItemId: strconv.Itoa(i)}
	}
	// rank items
	suite.RankingModel = newMockMatrixFactorizationForRecommend(10, 10)
	result, err := suite.rankByCollaborativeFiltering("1", [][]string{{"1", "2", "3", "4", "5"}})
	suite.NoError(err)
	suite.Equal([]string{"5", "4", "3", "2", "1"}, cache.RemoveScores(result))
	suite.IsDecreasing(cache.GetScores(result))
}

func (suite *WorkerTestSuite) TestRankByClickTroughRate() {
	ctx := context.Background()
	// insert a user
	err := suite.DataClient.BatchInsertUsers(ctx, []data.User{{UserId: "1"}})
	suite.NoError(err)
	// insert items
	itemCache := NewItemCache()
	for i := 1; i <= 5; i++ {
		itemCache.Set(strconv.Itoa(i), data.Item{ItemId: strconv.Itoa(i)})
	}
	// rank items
	suite.ClickModel = new(mockFactorizationMachine)
	result, err := suite.rankByClickTroughRate(&data.User{UserId: "1"}, [][]string{{"1", "2", "3", "4", "5"}}, itemCache)
	suite.NoError(err)
	suite.Equal([]string{"5", "4", "3", "2", "1"}, cache.RemoveScores(result))
	suite.IsDecreasing(cache.GetScores(result))
}

func (suite *WorkerTestSuite) TestReplacement_ClickThroughRate() {
	ctx := context.Background()
	suite.Config.Recommend.DataSource.PositiveFeedbackTypes = []string{"p"}
	suite.Config.Recommend.DataSource.ReadFeedbackTypes = []string{"n"}
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnablePopularRecommend = true
	suite.Config.Recommend.Replacement.EnableReplacement = true
	suite.Config.Recommend.Offline.EnableClickThroughPrediction = true

	// 1. Insert historical items into empty recommendation.
	// insert items
	err := suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "10"}, {ItemId: "9"}, {ItemId: "8"}, {ItemId: "7"}, {ItemId: "6"}, {ItemId: "5"},
	})
	suite.NoError(err)
	// insert feedback
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "p", UserId: "0", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "n", UserId: "0", ItemId: "9"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "i", UserId: "0", ItemId: "8"}},
	}, true, false, true)
	suite.NoError(err)
	suite.ClickModel = new(mockFactorizationMachine)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 10}, {"9", 9}}, recommends)

	// 2. Insert historical items into non-empty recommendation.
	err = suite.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastUpdateUserRecommendTime, "0"), time.Now().AddDate(-1, 0, 0)))
	suite.NoError(err)
	// insert popular items
	err = suite.CacheClient.SetSorted(ctx, cache.PopularItems, []cache.Scored{{"7", 10}, {"6", 9}, {"5", 8}})
	suite.NoError(err)
	// insert feedback
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "p", UserId: "0", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "n", UserId: "0", ItemId: "9"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "i", UserId: "0", ItemId: "8"}},
	}, true, false, true)
	suite.NoError(err)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 9}, {"9", 7.4}, {"7", 7}}, recommends)
}

func (suite *WorkerTestSuite) TestReplacement_CollaborativeFiltering() {
	ctx := context.Background()
	suite.Config.Recommend.DataSource.PositiveFeedbackTypes = []string{"p"}
	suite.Config.Recommend.DataSource.ReadFeedbackTypes = []string{"n"}
	suite.Config.Recommend.Offline.EnableColRecommend = false
	suite.Config.Recommend.Offline.EnablePopularRecommend = true
	suite.Config.Recommend.Replacement.EnableReplacement = true

	// 1. Insert historical items into empty recommendation.
	// insert items
	err := suite.DataClient.BatchInsertItems(ctx, []data.Item{
		{ItemId: "10"}, {ItemId: "9"}, {ItemId: "8"}, {ItemId: "7"}, {ItemId: "6"}, {ItemId: "5"},
	})
	suite.NoError(err)
	// insert feedback
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "p", UserId: "0", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "n", UserId: "0", ItemId: "9"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "i", UserId: "0", ItemId: "8"}},
	}, true, false, true)
	suite.NoError(err)
	suite.RankingModel = newMockMatrixFactorizationForRecommend(1, 10)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err := suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 10}, {"9", 9}}, recommends)

	// 2. Insert historical items into non-empty recommendation.
	err = suite.CacheClient.Set(ctx, cache.Time(cache.Key(cache.LastUpdateUserRecommendTime, "0"), time.Now().AddDate(-1, 0, 0)))
	suite.NoError(err)
	// insert popular items
	err = suite.CacheClient.SetSorted(ctx, cache.PopularItems, []cache.Scored{{"7", 10}, {"6", 9}, {"5", 8}})
	suite.NoError(err)
	// insert feedback
	err = suite.DataClient.BatchInsertFeedback(ctx, []data.Feedback{
		{FeedbackKey: data.FeedbackKey{FeedbackType: "p", UserId: "0", ItemId: "10"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "n", UserId: "0", ItemId: "9"}},
		{FeedbackKey: data.FeedbackKey{FeedbackType: "i", UserId: "0", ItemId: "8"}},
	}, true, false, true)
	suite.NoError(err)
	suite.Recommend([]data.User{{UserId: "0"}})
	recommends, err = suite.CacheClient.GetSorted(ctx, cache.Key(cache.OfflineRecommend, "0"), 0, 2)
	suite.NoError(err)
	suite.Equal([]cache.Scored{{"10", 9}, {"9", 7.4}, {"7", 7}}, recommends)
}

func (suite *WorkerTestSuite) TestHealth() {
	// ready
	req := httptest.NewRequest("GET", "https://example.com/", nil)
	w := httptest.NewRecorder()
	suite.checkLive(w, req)
	suite.Equal(http.StatusOK, w.Code)
	suite.Equal(marshal(suite.T(), HealthStatus{
		DataStoreError:      nil,
		CacheStoreError:     nil,
		DataStoreConnected:  true,
		CacheStoreConnected: true,
	}), w.Body.String())

	// not ready
	dataClient, cacheClient := suite.DataClient, suite.CacheClient
	suite.DataClient, suite.CacheClient = data.NoDatabase{}, cache.NoDatabase{}
	w = httptest.NewRecorder()
	suite.checkLive(w, req)
	suite.Equal(http.StatusOK, w.Code)
	suite.Equal(marshal(suite.T(), HealthStatus{
		DataStoreError:      data.ErrNoDatabase,
		CacheStoreError:     cache.ErrNoDatabase,
		DataStoreConnected:  false,
		CacheStoreConnected: false,
	}), w.Body.String())
	suite.DataClient, suite.CacheClient = dataClient, cacheClient
}

func TestWorker(t *testing.T) {
	suite.Run(t, new(WorkerTestSuite))
}
