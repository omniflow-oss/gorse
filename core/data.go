package core

import (
	"archive/zip"
	"bufio"
	"database/sql"
	"fmt"
	"github.com/zhenghaoz/gorse/base"
	"gonum.org/v1/gonum/stat"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type DataSetInterface interface {
	// GlobalMean returns the global mean of ratings in the dataset.
	GlobalMean() float64
	// Count returns the number of ratings in the dataset.
	Count() int
	// UserCount returns the number of users in the dataset.
	UserCount() int
	// ItemCount returns the number of items in the dataset.
	ItemCount() int
	// Get i-th rating by (user ID, item ID, rating).
	Get(i int) (int, int, float64)
	// GetWithIndex gets i-th rating by (user index, item index, rating).
	GetWithIndex(i int) (int, int, float64)
	// UserIndexer returns the user indexer.
	UserIndexer() *base.Indexer
	// ItemIndexer returns the item indexer.
	ItemIndexer() *base.Indexer
	// SubSet gets a subset of current dataset.
	SubSet(subset []int) DataSetInterface
	// Users returns subsets of users.
	Users() []*base.MarginalSubSet
	// Items returns subsets of items.
	Items() []*base.MarginalSubSet
	// User returns the subset of a user.
	User(userId int) *base.MarginalSubSet
	// Item returns the subset of a item.
	Item(itemId int) *base.MarginalSubSet
	// UserByIndex returns the subset of a user by the index.
	UserByIndex(userIndex int) *base.MarginalSubSet
	// ItemByIndex returns the subset of a item by the index.
	ItemByIndex(itemIndex int) *base.MarginalSubSet
}

// DataSet contains preprocessed data structures for recommendation models.
type DataSet struct {
	ratings     []float64              // ratings
	userIDs     []int                  // user IDs
	itemIDs     []int                  // item IDs
	userIndices []int                  // user indices
	itemIndices []int                  // item indices
	userIndexer *base.Indexer          // user indexer
	itemIndexer *base.Indexer          // item indexer
	users       []*base.MarginalSubSet // subsets of users' ratings
	items       []*base.MarginalSubSet // subsets of items' ratings
}

// NewDataSet creates a data set.
func NewDataSet(userIDs, itemIDs []int, ratings []float64) *DataSet {
	set := new(DataSet)
	set.userIDs = userIDs
	set.itemIDs = itemIDs
	set.ratings = ratings
	// Index users and items
	set.userIndexer = base.NewIndexer()
	set.itemIndexer = base.NewIndexer()
	set.userIndices = make([]int, set.Count())
	set.itemIndices = make([]int, set.Count())
	for i := 0; i < set.Count(); i++ {
		userId := set.userIDs[i]
		itemId := set.itemIDs[i]
		set.userIndexer.Add(userId)
		set.itemIndexer.Add(itemId)
		set.userIndices[i] = set.userIndexer.ToIndex(userId)
		set.itemIndices[i] = set.itemIndexer.ToIndex(itemId)
	}
	// Index users' ratings and items' ratings
	userSubsetIndices := base.NewMatrixInt(set.UserCount(), 0)
	itemSubsetIndices := base.NewMatrixInt(set.ItemCount(), 0)
	for i := 0; i < set.Count(); i++ {
		userId := set.userIDs[i]
		itemId := set.itemIDs[i]
		userIndex := set.userIndexer.ToIndex(userId)
		itemIndex := set.itemIndexer.ToIndex(itemId)
		userSubsetIndices[userIndex] = append(userSubsetIndices[userIndex], i)
		itemSubsetIndices[itemIndex] = append(itemSubsetIndices[itemIndex], i)
	}
	set.users = make([]*base.MarginalSubSet, set.UserCount())
	set.items = make([]*base.MarginalSubSet, set.ItemCount())
	for u := range set.users {
		set.users[u] = base.NewMarginalSubSet(set.itemIDs, set.itemIndices, ratings, userSubsetIndices[u])
	}
	for i := range set.items {
		set.items[i] = base.NewMarginalSubSet(set.userIDs, set.userIndices, ratings, itemSubsetIndices[i])
	}
	return set
}

// Count returns the number of ratings.
func (set *DataSet) Count() int {
	if set == nil {
		return 0
	}
	return len(set.ratings)
}

// Get the i-th record by <user ID, item ID, rating>.
func (set *DataSet) Get(i int) (int, int, float64) {
	return set.userIDs[i], set.itemIDs[i], set.ratings[i]
}

// GetWithIndex gets the i-th record by <user index, item index, rating>.
func (set *DataSet) GetWithIndex(i int) (int, int, float64) {
	return set.userIndices[i], set.itemIndices[i], set.ratings[i]
}

// GlobalMean computes the global mean of ratings.
func (set *DataSet) GlobalMean() float64 {
	return stat.Mean(set.ratings, nil)
}

// UserIndexer returns the user indexer.
func (set *DataSet) UserIndexer() *base.Indexer {
	return set.userIndexer
}

// ItemIndexer returns the item indexer.
func (set *DataSet) ItemIndexer() *base.Indexer {
	return set.itemIndexer
}

// UserCount returns the number of users.
func (set *DataSet) UserCount() int {
	return set.UserIndexer().Len()
}

// ItemCount returns the number of items.
func (set *DataSet) ItemCount() int {
	return set.ItemIndexer().Len()
}

// SubSet returns a subset of current dataset.
func (set *DataSet) SubSet(subset []int) DataSetInterface {
	return NewSubSet(set, subset)
}

// UserByIndex gets ratings of a user by index.
func (set *DataSet) UserByIndex(userIndex int) *base.MarginalSubSet {
	return set.users[userIndex]
}

// ItemByIndex gets ratings of a item by index.
func (set *DataSet) ItemByIndex(itemIndex int) *base.MarginalSubSet {
	return set.items[itemIndex]
}

// Users gets ratings of a user by index.
func (set *DataSet) Users() []*base.MarginalSubSet {
	return set.users
}

// Items gets ratings of a item by index.
func (set *DataSet) Items() []*base.MarginalSubSet {
	return set.items
}

func (set *DataSet) User(userId int) *base.MarginalSubSet {
	userIndex := set.userIndexer.ToIndex(userId)
	return set.UserByIndex(userIndex)
}

func (set *DataSet) Item(itemId int) *base.MarginalSubSet {
	itemIndex := set.itemIndexer.ToIndex(itemId)
	return set.ItemByIndex(itemIndex)
}

// SubSet creates a subset index over a existed dataset.
type SubSet struct {
	*DataSet                        // the existed dataset.
	subset   []int                  // indices of subset's samples
	users    []*base.MarginalSubSet // subsets of users' ratings
	items    []*base.MarginalSubSet // subsets of items' ratings
}

// NewSubSet creates a subset of a dataset.
func NewSubSet(dataSet *DataSet, subset []int) DataSetInterface {
	set := new(SubSet)
	set.DataSet = dataSet
	set.subset = subset
	// Index users' ratings and items' ratings
	userSubsetIndices := base.NewMatrixInt(set.UserCount(), 0)
	itemSubsetIndices := base.NewMatrixInt(set.ItemCount(), 0)
	for _, i := range set.subset {
		userId := set.userIDs[i]
		itemId := set.itemIDs[i]
		userIndex := set.userIndexer.ToIndex(userId)
		itemIndex := set.itemIndexer.ToIndex(itemId)
		userSubsetIndices[userIndex] = append(userSubsetIndices[userIndex], i)
		itemSubsetIndices[itemIndex] = append(itemSubsetIndices[itemIndex], i)
	}
	set.users = make([]*base.MarginalSubSet, set.UserCount())
	set.items = make([]*base.MarginalSubSet, set.ItemCount())
	for u := range set.users {
		set.users[u] = base.NewMarginalSubSet(set.itemIDs, set.itemIndices, set.ratings, userSubsetIndices[u])
	}
	for i := range set.items {
		set.items[i] = base.NewMarginalSubSet(set.userIDs, set.userIndices, set.ratings, itemSubsetIndices[i])
	}
	return set
}

// Count returns the number of ratings.
func (set *SubSet) Count() int {
	if set == nil {
		return 0
	}
	return len(set.subset)
}

// Get the i-th record by <user ID, item ID, rating>.
func (set *SubSet) Get(i int) (int, int, float64) {
	return set.DataSet.Get(set.subset[i])
}

// GetWithIndex gets the i-th record by <user index, item index, rating>.
func (set *SubSet) GetWithIndex(i int) (int, int, float64) {
	return set.DataSet.GetWithIndex(set.subset[i])
}

// GlobalMean computes the global mean of ratings.
func (set *SubSet) GlobalMean() float64 {
	sum := 0.0
	for i := 0; i < set.Count(); i++ {
		_, _, rating := set.Get(i)
		sum += rating
	}
	return sum / float64(set.Count())
}

// SubSet returns a subset of current dataset.
func (set *SubSet) SubSet(indices []int) DataSetInterface {
	rawIndices := make([]int, len(indices))
	for i, index := range indices {
		rawIndices[i] = set.subset[index]
	}
	return NewSubSet(set.DataSet, rawIndices)
}

// UserByIndex gets ratings of a user by index.
func (set *SubSet) UserByIndex(userIndex int) *base.MarginalSubSet {
	return set.users[userIndex]
}

// ItemByIndex gets ratings of a item by index.
func (set *SubSet) ItemByIndex(itemIndex int) *base.MarginalSubSet {
	return set.items[itemIndex]
}

// Users gets ratings of a user by index.
func (set *SubSet) Users() []*base.MarginalSubSet {
	return set.users
}

// Items gets ratings of a item by index.
func (set *SubSet) Items() []*base.MarginalSubSet {
	return set.items
}

func (set *SubSet) User(userId int) *base.MarginalSubSet {
	userIndex := set.userIndexer.ToIndex(userId)
	return set.UserByIndex(userIndex)
}

func (set *SubSet) Item(itemId int) *base.MarginalSubSet {
	itemIndex := set.itemIndexer.ToIndex(itemId)
	return set.ItemByIndex(itemIndex)
}

/* Loader */

// LoadDataFromBuiltIn loads a built-in Data set. Now support:
//   ml-100k   - MovieLens 100K
//   ml-1m     - MovieLens 1M
//   ml-10m    - MovieLens 10M
//   ml-20m    - MovieLens 20M
//   netflix   - Netflix
//   filmtrust - FlimTrust
//   epinions  - Epinions
func LoadDataFromBuiltIn(dataSetName string) *DataSet {
	// Extract Data set information
	dataSet, exist := builtInDataSets[dataSetName]
	if !exist {
		log.Fatal("no such Data set ", dataSetName)
	}
	dataFileName := filepath.Join(dataSetDir, dataSet.path)
	if _, err := os.Stat(dataFileName); os.IsNotExist(err) {
		zipFileName, _ := downloadFromUrl(dataSet.url, downloadDir)
		if _, err := unzip(zipFileName, dataSetDir); err != nil {
			panic(err)
		}
	}
	return dataSet.loader(dataFileName, dataSet.sep, dataSet.header)
}

// LoadDataFromCSV loads Data from a CSV file. The CSV file should be:
//   [optional header]
//   <userId 1> <sep> <itemId 1> <sep> <rating 1> <sep> <extras>
//   <userId 2> <sep> <itemId 2> <sep> <rating 2> <sep> <extras>
//   <userId 3> <sep> <itemId 3> <sep> <rating 3> <sep> <extras>
//   ...
// For example, the `u.Data` from MovieLens 100K is:
//  196\t242\t3\t881250949
//  186\t302\t3\t891717742
//  22\t377\t1\t878887116
func LoadDataFromCSV(fileName string, sep string, hasHeader bool) *DataSet {
	users := make([]int, 0)
	items := make([]int, 0)
	ratings := make([]float64, 0)
	// Open file
	file, err := os.Open(fileName)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	// Read CSV file
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Ignore header
		if hasHeader {
			hasHeader = false
			continue
		}
		fields := strings.Split(line, sep)
		// Ignore empty line
		if len(fields) < 2 {
			continue
		}
		user, _ := strconv.Atoi(fields[0])
		item, _ := strconv.Atoi(fields[1])
		rating, _ := strconv.ParseFloat(fields[2], 32)
		users = append(users, user)
		items = append(items, item)
		ratings = append(ratings, rating)
	}
	return NewDataSet(users, items, ratings)
}

// LoadDataFromNetflix loads Data from the Netflix dataset. The file should be:
//   <itemId 1>:
//   <userId 1>, <rating 1>, <date>
//   <userId 2>, <rating 2>, <date>
//   <userId 3>, <rating 3>, <date>
//   ...
func LoadDataFromNetflix(fileName string, _ string, _ bool) *DataSet {
	users := make([]int, 0)
	items := make([]int, 0)
	ratings := make([]float64, 0)
	// Open file
	file, err := os.Open(fileName)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	// Read file
	scanner := bufio.NewScanner(file)
	itemId := -1
	for scanner.Scan() {
		line := scanner.Text()
		if line[len(line)-1] == ':' {
			// <itemId>:
			if itemId, err = strconv.Atoi(line[0 : len(line)-1]); err != nil {
				log.Fatal(err)
			}
		} else {
			// <userId>, <rating>, <date>
			fields := strings.Split(line, ",")
			userId, _ := strconv.Atoi(fields[0])
			rating, _ := strconv.Atoi(fields[1])
			users = append(users, userId)
			items = append(items, itemId)
			ratings = append(ratings, float64(rating))
		}
	}
	return NewDataSet(users, items, ratings)
}

// LoadDataFromSQL loads data from a SQL database.
func LoadDataFromSQL(db *sql.DB, tableName string,
	userColumn string, itemColumn string, ratingColumn string) (*DataSet, error) {
	users := make([]int, 0)
	items := make([]int, 0)
	ratings := make([]float64, 0)
	query := fmt.Sprintf("SELECT %s, %s, %s FROM %s;",
		userColumn, itemColumn, ratingColumn, tableName)
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var itemId, userId int
		var rating float64
		err = rows.Scan(&userId, &itemId, &rating)
		if err != nil {
			return nil, err
		}
		users = append(users, userId)
		items = append(items, itemId)
		ratings = append(ratings, float64(rating))
	}
	return NewDataSet(users, items, ratings), nil
}

/* Utils */

// downloadFromUrl downloads file from URL.
func downloadFromUrl(src string, dst string) (string, error) {
	fmt.Printf("Download dataset from %s\n", src)
	// Extract file name
	tokens := strings.Split(src, "/")
	fileName := filepath.Join(dst, tokens[len(tokens)-1])
	// Create file
	if err := os.MkdirAll(filepath.Dir(fileName), os.ModePerm); err != nil {
		return fileName, err
	}
	output, err := os.Create(fileName)
	if err != nil {
		fmt.Println("Error while creating", fileName, "-", err)
		return fileName, err
	}
	defer output.Close()
	// Download file
	response, err := http.Get(src)
	if err != nil {
		fmt.Println("Error while downloading", src, "-", err)
		return fileName, err
	}
	defer response.Body.Close()
	// Save file
	_, err = io.Copy(output, response.Body)
	if err != nil {
		fmt.Println("Error while downloading", src, "-", err)
		return fileName, err
	}
	return fileName, nil
}

// unzip zip file.
func unzip(src string, dst string) ([]string, error) {
	fmt.Printf("Unzip dataset %s\n", src)
	var fileNames []string
	// Open zip file
	r, err := zip.OpenReader(src)
	if err != nil {
		return fileNames, err
	}
	defer r.Close()
	// Extract files
	for _, f := range r.File {
		// Open file
		rc, err := f.Open()
		if err != nil {
			return fileNames, err
		}
		// Store filename/path for returning and using later on
		filePath := filepath.Join(dst, f.Name)
		// Check for ZipSlip. More Info: http://bit.ly/2MsjAWE
		if !strings.HasPrefix(filePath, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fileNames, fmt.Errorf("%s: illegal file path", filePath)
		}
		// Add filename
		fileNames = append(fileNames, filePath)
		if f.FileInfo().IsDir() {
			// Create folder
			if err = os.MkdirAll(filePath, os.ModePerm); err != nil {
				return fileNames, err
			}
		} else {
			// Create all folders
			if err = os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
				return fileNames, err
			}
			// Create file
			outFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return fileNames, err
			}
			// Save file
			_, err = io.Copy(outFile, rc)
			// Close the file without defer to close before next iteration of loop
			outFile.Close()
			if err != nil {
				return fileNames, err
			}
		}
		// Close file
		rc.Close()
	}
	return fileNames, nil
}
