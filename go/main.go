package main

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "net/http/pprof"

	"github.com/francoispqt/gojay"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	jsoniter "github.com/json-iterator/go"
	goji "goji.io"
	"goji.io/pat"
	"golang.org/x/crypto/bcrypt"
)

var json = jsoniter.Config{
	EscapeHTML:                    false,
	ObjectFieldMustBeSimpleString: true,
}.Froze()

const (
	sessionName = "session_isucari"

	DefaultPaymentServiceURL  = "http://localhost:5555"
	DefaultShipmentServiceURL = "http://localhost:7000"

	ItemMinPrice    = 100
	ItemMaxPrice    = 1000000
	ItemPriceErrMsg = "商品価格は100ｲｽｺｲﾝ以上、1,000,000ｲｽｺｲﾝ以下にしてください"

	ItemStatusOnSale  = "on_sale"
	ItemStatusTrading = "trading"
	ItemStatusSoldOut = "sold_out"

	PaymentServiceIsucariAPIKey = "a15400e46c83635eb181-946abb51ff26a868317c"
	PaymentServiceIsucariShopID = "11"

	TransactionEvidenceStatusWaitShipping = "wait_shipping"
	TransactionEvidenceStatusWaitDone     = "wait_done"
	TransactionEvidenceStatusDone         = "done"

	ShippingsStatusInitial    = "initial"
	ShippingsStatusWaitPickup = "wait_pickup"
	ShippingsStatusShipping   = "shipping"
	ShippingsStatusDone       = "done"

	BumpChargeSeconds = 3 * time.Second

	ItemsPerPage        = 48
	TransactionsPerPage = 10

	BcryptCost = 4
)

var (
	dbx                      *sqlx.DB
	categories               []Category
	doneTransactionEvidences map[int64]struct{}
	buyingMutexMap           BuyingMutexMap
	itemsPool                = NewItemsPool(ItemsPerPage + 1)
	itemsTPool               = NewItemsPool(TransactionsPerPage + 1)
	itemEPool                = NewItemEPool()

	usersCache = NewUserCacheMap()

	paymentServiceURL  = DefaultPaymentServiceURL
	shipmentServiceURL = DefaultShipmentServiceURL
)

const UserCacheMapShards = 256

type UserCacheMapShard struct {
	users map[int64]User
	sync.RWMutex
}
type UserCacheMap []*UserCacheMapShard

func NewUserCacheMap() UserCacheMap {
	m := make(UserCacheMap, UserCacheMapShards)
	for i := 0; i < UserCacheMapShards; i++ {
		shard := NewUserCacheMapShard()
		m[i] = &shard
	}
	return m
}
func NewUserCacheMapShard() UserCacheMapShard {
	users := make(map[int64]User, 50)
	return UserCacheMapShard{users: users}
}

func (m UserCacheMap) GetShard(id int64) *UserCacheMapShard {
	return m[id%UserCacheMapShards]
}
func (m UserCacheMap) Get(id int64) (User, bool) {
	s := m.GetShard(id)
	s.RLock()
	u, ok := s.users[id]
	s.RUnlock()
	return u, ok
}
func (m UserCacheMap) Set(id int64, u User) {
	s := m.GetShard(id)
	s.Lock()
	s.users[id] = u
	s.Unlock()
}
func (m UserCacheMap) GetByAccountName(name string) (User, bool) {
	for i := range m {
		s := m[i]

		s.RLock()
		for j := range s.users {
			if s.users[j].AccountName == name {
				s.RUnlock()
				return s.users[j], true
			}
		}
		s.RUnlock()
	}
	return User{}, false
}
func (m UserCacheMap) Lock(id int64) {
	s := m.GetShard(id)
	s.Lock()
}
func (m UserCacheMap) LockAll() {
	for i := range m {
		s := m[i]
		s.Lock()
	}
}
func (m UserCacheMap) Unlock(id int64) {
	s := m.GetShard(id)
	s.Unlock()
}
func (m UserCacheMap) UnlockAll() {
	for i := range m {
		s := m[i]
		s.Unlock()
	}
}
func (m UserCacheMap) GetWithoutLock(id int64) (User, bool) {
	s := m.GetShard(id)
	u, ok := s.users[id]
	return u, ok
}
func (m UserCacheMap) SetWithoutLock(id int64, u User) {
	s := m.GetShard(id)
	s.users[id] = u
}

type User struct {
	ID             int64     `json:"id" db:"id"`
	AccountName    string    `json:"account_name" db:"account_name"`
	HashedPassword []byte    `json:"-" db:"hashed_password"`
	Address        string    `json:"address,omitempty" db:"address"`
	NumSellItems   int       `json:"num_sell_items" db:"num_sell_items"`
	LastBump       time.Time `json:"-" db:"last_bump"`
	CreatedAt      time.Time `json:"-" db:"created_at"`
}

type UserSimple struct {
	ID           int64  `json:"id" db:"id"`
	AccountName  string `json:"account_name" db:"account_name"`
	NumSellItems int    `json:"num_sell_items" db:"num_sell_items"`
}

func (u *UserSimple) MarshalJSONObject(enc *gojay.Encoder) {
	enc.Int64Key("id", u.ID)
	enc.StringKey("account_name", u.AccountName)
	enc.IntKey("num_sell_items", u.NumSellItems)
}
func (u *UserSimple) IsNil() bool {
	return u == nil
}

type ItemsPool struct {
	p sync.Pool
}

func NewItemsPool(length int) ItemsPool {
	return ItemsPool{
		p: sync.Pool{
			New: func() interface{} {
				return make([]Item, 0, length)
			},
		},
	}
}
func (p *ItemsPool) Get() []Item {
	return p.p.Get().([]Item)
}
func (p *ItemsPool) Put(i []Item) {
	i = i[:0]
	p.p.Put(i)
}

type Item struct {
	ID               int64     `json:"id" db:"id"`
	SellerID         int64     `json:"seller_id" db:"seller_id"`
	BuyerID          int64     `json:"buyer_id" db:"buyer_id"`
	Status           string    `json:"status" db:"status"`
	Name             string    `json:"name" db:"name"`
	Price            int       `json:"price" db:"price"`
	Description      string    `json:"description" db:"description"`
	ImageName        string    `json:"image_name" db:"image_name"`
	CategoryID       int       `json:"category_id" db:"category_id"`
	ParentCategoryID int       `json:"-" db:"parent_category_id"`
	CreatedAt        time.Time `json:"-" db:"created_at"`
	UpdatedAt        time.Time `json:"-" db:"updated_at"`
}

type ItemSimple struct {
	ID         int64       `json:"id"`
	SellerID   int64       `json:"seller_id"`
	Seller     *UserSimple `json:"seller"`
	Status     string      `json:"status"`
	Name       string      `json:"name"`
	Price      int         `json:"price"`
	ImageURL   string      `json:"image_url"`
	CategoryID int         `json:"category_id"`
	Category   *Category   `json:"category"`
	CreatedAt  int64       `json:"created_at"`
}

func (i *ItemSimple) MarshalJSONObject(enc *gojay.Encoder) {
	enc.Int64Key("id", i.ID)
	enc.Int64Key("seller_id", i.SellerID)
	enc.ObjectKey("seller", i.Seller)
	enc.StringKey("status", i.Status)
	enc.StringKey("name", i.Name)
	enc.IntKey("price", i.Price)
	enc.StringKey("image_url", i.ImageURL)
	enc.IntKey("category_id", i.CategoryID)
	enc.ObjectKey("category", i.Category)
	enc.Int64Key("created_at", i.CreatedAt)
}
func (i *ItemSimple) IsNil() bool {
	return i == nil
}

type ItemSimples []*ItemSimple

func (is *ItemSimples) MarshalJSONArray(enc *gojay.Encoder) {
	for _, e := range *is {
		enc.Object(e)
	}
}
func (is *ItemSimples) IsNil() bool {
	return len(*is) == 0
}

type ItemDetail struct {
	ID                        int64       `json:"id"`
	SellerID                  int64       `json:"seller_id"`
	Seller                    *UserSimple `json:"seller"`
	BuyerID                   int64       `json:"buyer_id,omitempty"`
	Buyer                     *UserSimple `json:"buyer,omitempty"`
	Status                    string      `json:"status"`
	Name                      string      `json:"name"`
	Price                     int         `json:"price"`
	Description               string      `json:"description"`
	ImageURL                  string      `json:"image_url"`
	CategoryID                int         `json:"category_id"`
	Category                  *Category   `json:"category"`
	TransactionEvidenceID     int64       `json:"transaction_evidence_id,omitempty"`
	TransactionEvidenceStatus string      `json:"transaction_evidence_status,omitempty"`
	ShippingStatus            string      `json:"shipping_status,omitempty"`
	CreatedAt                 int64       `json:"created_at"`
}

func (i *ItemDetail) MarshalJSONObject(enc *gojay.Encoder) {
	enc.Int64Key("id", i.ID)
	enc.Int64Key("seller_id", i.SellerID)
	enc.ObjectKey("seller", i.Seller)
	enc.Int64KeyOmitEmpty("buyer_id", i.BuyerID)
	enc.ObjectKeyOmitEmpty("buyer", i.Buyer)
	enc.StringKey("status", i.Status)
	enc.StringKey("name", i.Name)
	enc.IntKey("price", i.Price)
	enc.StringKey("description", i.Description)
	enc.StringKey("image_url", i.ImageURL)
	enc.IntKey("category_id", i.CategoryID)
	enc.ObjectKey("category", i.Category)
	enc.Int64KeyOmitEmpty("transaction_evidence_id", i.TransactionEvidenceID)
	enc.StringKeyOmitEmpty("transaction_evidence_status", i.TransactionEvidenceStatus)
	enc.StringKeyOmitEmpty("shipping_status", i.ShippingStatus)
	enc.Int64Key("created_at", i.CreatedAt)
}
func (i *ItemDetail) IsNil() bool {
	return i == nil
}

type ItemDetails []*ItemDetail

func (id *ItemDetails) MarshalJSONArray(enc *gojay.Encoder) {
	for _, e := range *id {
		enc.Object(e)
	}
}
func (id *ItemDetails) IsNil() bool {
	return len(*id) == 0
}

type TransactionEvidence struct {
	ID                 int64     `json:"id" db:"id"`
	SellerID           int64     `json:"seller_id" db:"seller_id"`
	BuyerID            int64     `json:"buyer_id" db:"buyer_id"`
	Status             string    `json:"status" db:"status"`
	ItemID             int64     `json:"item_id" db:"item_id"`
}

type Shipping struct {
	TransactionEvidenceID int64     `json:"transaction_evidence_id" db:"transaction_evidence_id"`
	Status                string    `json:"status" db:"status"`
	ReserveID             string    `json:"reserve_id" db:"reserve_id"`
	ReserveTime           int64     `json:"reserve_time" db:"reserve_time"`
	ImgBinary             []byte    `json:"-" db:"img_binary"`
}

type Category struct {
	ID                 int    `json:"id" db:"id"`
	ParentID           int    `json:"parent_id" db:"parent_id"`
	CategoryName       string `json:"category_name" db:"category_name"`
	ParentCategoryName string `json:"parent_category_name,omitempty" db:"-"`
}

func (c *Category) MarshalJSONObject(enc *gojay.Encoder) {
	enc.IntKey("id", c.ID)
	enc.IntKey("parent_id", c.ParentID)
	enc.StringKey("category_name", c.CategoryName)
	enc.StringKeyOmitEmpty("parent_category_name", c.ParentCategoryName)
}
func (c *Category) IsNil() bool {
	return c == nil
}

type reqInitialize struct {
	PaymentServiceURL  string `json:"payment_service_url"`
	ShipmentServiceURL string `json:"shipment_service_url"`
}

type resInitialize struct {
	Campaign int    `json:"campaign"`
	Language string `json:"language"`
}

type resNewItems struct {
	RootCategoryID   int          `json:"root_category_id,omitempty"`
	RootCategoryName string       `json:"root_category_name,omitempty"`
	HasNext          bool         `json:"has_next"`
	Items            *ItemSimples `json:"items"`
}

func (r *resNewItems) MarshalJSONObject(enc *gojay.Encoder) {
	enc.IntKeyOmitEmpty("root_category_id", r.RootCategoryID)
	enc.StringKeyOmitEmpty("root_category_name", r.RootCategoryName)
	enc.BoolKey("has_next", r.HasNext)
	enc.ArrayKey("items", r.Items)
}
func (r *resNewItems) IsNil() bool {
	return r == nil
}

type resUserItems struct {
	User    *UserSimple  `json:"user"`
	HasNext bool         `json:"has_next"`
	Items   *ItemSimples `json:"items"`
}

func (r *resUserItems) MarshalJSONObject(enc *gojay.Encoder) {
	enc.ObjectKey("user", r.User)
	enc.BoolKey("has_next", r.HasNext)
	enc.ArrayKey("items", r.Items)
}
func (r *resUserItems) IsNil() bool {
	return r == nil
}

type resTransactions struct {
	HasNext bool         `json:"has_next"`
	Items   *ItemDetails `json:"items"`
}

func (r *resTransactions) MarshalJSONObject(enc *gojay.Encoder) {
	enc.BoolKey("has_next", r.HasNext)
	enc.ArrayKey("items", r.Items)
}
func (r *resTransactions) IsNil() bool {
	return r == nil
}

type reqRegister struct {
	AccountName string `json:"account_name"`
	Address     string `json:"address"`
	Password    string `json:"password"`
}

type reqLogin struct {
	AccountName string `json:"account_name"`
	Password    string `json:"password"`
}

type reqItemEdit struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
	ItemPrice int    `json:"item_price"`
}

type resItemEdit struct {
	ItemID        int64 `json:"item_id"`
	ItemPrice     int   `json:"item_price"`
	ItemCreatedAt int64 `json:"item_created_at"`
	ItemUpdatedAt int64 `json:"item_updated_at"`
}

type reqBuy struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
	Token     string `json:"token"`
}

type resBuy struct {
	TransactionEvidenceID int64 `json:"transaction_evidence_id"`
}

type resSell struct {
	ID int64 `json:"id"`
}

type reqPostShip struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type resPostShip struct {
	Path      string `json:"path"`
	ReserveID string `json:"reserve_id"`
}

type reqPostShipDone struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type reqPostComplete struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type reqBump struct {
	CSRFToken string `json:"csrf_token"`
	ItemID    int64  `json:"item_id"`
}

type resSetting struct {
	CSRFToken         string     `json:"csrf_token"`
	PaymentServiceURL string     `json:"payment_service_url"`
	User              *User      `json:"user,omitempty"`
	Categories        []Category `json:"categories"`
}

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func main() {
	host := os.Getenv("MYSQL_HOST")
	if host == "" {
		host = "172.16.0.162"
		// host = "127.0.0.1"
	}
	port := os.Getenv("MYSQL_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("failed to read DB port number from an environment variable MYSQL_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("MYSQL_USER")
	if user == "" {
		user = "isucari"
	}
	dbname := os.Getenv("MYSQL_DBNAME")
	if dbname == "" {
		dbname = "isucari"
	}
	password := os.Getenv("MYSQL_PASS")
	if password == "" {
		password = "isucari"
	}

	go http.ListenAndServe(":6060", nil)

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&interpolateParams=true",
		user,
		password,
		host,
		port,
		dbname,
	)

	dbx, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("failed to connect to DB: %s.", err.Error())
	}
	defer dbx.Close()

	dbx.SetMaxIdleConns(1024)
	dbx.SetConnMaxLifetime(0)

	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1024
	http.DefaultTransport.(*http.Transport).ForceAttemptHTTP2 = true
	http.DefaultClient.Timeout = 5 * time.Second

	mux := goji.NewMux()

	coala := coalaRoute("GET")

	// API
	mux.HandleFunc(pat.Post("/initialize"), postInitialize)
	mux.HandleFunc(pat.Get("/new_items.json"), getNewItems)
	mux.HandleFunc(pat.Get("/new_items/:root_category_id.json"), getNewCategoryItems)
	mux.HandleFunc(pat.Get("/users/transactions.json"), getTransactions)
	mux.HandleFunc(pat.Get("/users/:user_id.json"), getUserItems)
	mux.HandleFunc(pat.Get("/items/:item_id.json"), getItem)
	mux.HandleFunc(pat.Post("/items/edit"), postItemEdit)
	mux.HandleFunc(pat.Post("/buy"), postBuy)
	mux.HandleFunc(pat.Post("/sell"), postSell)
	mux.HandleFunc(pat.Post("/ship"), postShip)
	mux.HandleFunc(pat.Post("/ship_done"), postShipDone)
	mux.HandleFunc(pat.Post("/complete"), postComplete)
	mux.HandleFunc(pat.Get("/transactions/:transaction_evidence_id.png"), getQRCode)
	mux.HandleFunc(pat.Post("/bump"), postBump)
	mux.HandleFunc(pat.Get("/settings"), getSettings)
	mux.HandleFunc(pat.Post("/login"), postLogin)
	mux.HandleFunc(pat.Post("/register"), postRegister)
	mux.HandleFunc(pat.Get("/reports.json"), getReports)
	mux.Use(coala)

	log.Fatal(http.ListenAndServe(":8000", mux))
}

func getCSRFToken(r *http.Request) string {
	data, ok := getSession(r)
	if !ok {
		return ""
	}
	return data.CsrfToken
}

func getUser(r *http.Request) (user User, errCode int, errMsg string) {
	data, ok := getSession(r)
	if !ok {
		return user, http.StatusNotFound, "no session"
	}

	user, ok = usersCache.Get(data.UserID)
	if !ok {
		return user, http.StatusNotFound, "user not found"
	}

	return user, http.StatusOK, ""
}

func getUserID(r *http.Request) (userID int64, errCode int, errMsg string) {
	data, ok := getSession(r)
	if !ok {
		return 0, http.StatusNotFound, "no session"
	}

	return data.UserID, http.StatusOK, ""
}

func getUserSimpleByID(userID int64) (userSimple UserSimple, err error) {
	user, ok := usersCache.Get(userID)
	if !ok {
		return userSimple, fmt.Errorf("User not found")
	}

	userSimple.ID = user.ID
	userSimple.AccountName = user.AccountName
	userSimple.NumSellItems = user.NumSellItems
	return userSimple, nil
}

func getCategoryByID(categoryID int) (Category, error) {
	for i := range categories {
		if categories[i].ID == categoryID {
			return categories[i], nil
		}
	}
	return Category{}, errors.New("nothing category")
}

func postInitialize(w http.ResponseWriter, r *http.Request) {
	ri := reqInitialize{}

	err := json.NewDecoder(r.Body).Decode(&ri)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	cmd := exec.Command("../sql/init.sh")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr
	cmd.Run()
	if err != nil {
		outputErrorMsg(w, http.StatusInternalServerError, "exec init.sh error")
		return
	}

	// キャッシュのリセット
	doneTransactionEvidences = make(map[int64]struct{}, 100)
	buyingMutexMap = NewBuyingMutexMap()

	// configのメモリキャッシュ
	if ri.PaymentServiceURL != "" {
		paymentServiceURL = ri.PaymentServiceURL
	}
	if ri.ShipmentServiceURL != "" {
		shipmentServiceURL = ri.ShipmentServiceURL
	}

	// categoryのメモリキャッシュ
	categories = []Category{}
	err = dbx.Select(&categories, "SELECT * FROM `categories`")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "category mem cache error")
		return
	}
	for i := range categories {
		if categories[i].ParentID == 0 {
			continue
		}

		for j := range categories {
			if categories[i].ParentID == categories[j].ID {
				categories[i].ParentCategoryName = categories[j].CategoryName
				break
			}
		}
	}

	// userのメモリキャッシュ
	users := []User{}
	err = dbx.Select(&users, "SELECT * FROM `users`")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "user mem cache error")
		return
	}
	usersCache = NewUserCacheMap()
	usersCache.LockAll()
	for _, u := range users {
		usersCache.SetWithoutLock(u.ID, u)
	}
	usersCache.UnlockAll()

	campaign, err := strconv.Atoi(os.Getenv("CAMPAIGN"))
	if err != nil {
		campaign = 0
	}
	res := resInitialize{
		// キャンペーン実施時には還元率の設定を返す。詳しくはマニュアルを参照のこと。
		Campaign: campaign,
		// 実装言語を返す
		Language: "Go",
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(res)
}

func getNewItems(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	var err error
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := itemsPool.Get()
	if itemID > 0 && createdAt > 0 {
		// paging
		err := dbx.Select(&items,
			"SELECT id, seller_id, name, price, image_name, category_id, created_at FROM `items` WHERE `status` = ? AND (`created_at` < ?  OR (`created_at` = ? AND `id` < ?)) ORDER BY `created_at` DESC LIMIT ?",
			ItemStatusOnSale,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		// 1st page
		err := dbx.Select(&items,
			"SELECT id, seller_id, name, price, image_name, category_id, created_at FROM `items` WHERE `status` = ? ORDER BY `created_at` DESC LIMIT ?",
			ItemStatusOnSale,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	itemSimples := make(ItemSimples, len(items))
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		return
	}

	for i := range items {
		seller, err := getUserSimpleByID(items[i].SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(items[i].CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples[i] = &ItemSimple{
			ID:         items[i].ID,
			SellerID:   items[i].SellerID,
			Seller:     &seller,
			Status:     ItemStatusOnSale,
			Name:       items[i].Name,
			Price:      items[i].Price,
			ImageURL:   getImageURL(items[i].ImageName),
			CategoryID: items[i].CategoryID,
			Category:   &category,
			CreatedAt:  items[i].CreatedAt.Unix(),
		}
	}

	itemsPool.Put(items)

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		Items:   &itemSimples,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	gojay.NewEncoder(w).Encode(&rni)
	if err != nil {
		log.Print(err)
	}

}

func getNewCategoryItems(w http.ResponseWriter, r *http.Request) {
	// /new_items/:root_category_id.json
	prefixLen := len("/new_items/")
	suffixLen := len(".json")
	rootCategoryIDStr := r.URL.Path[prefixLen : len(r.URL.Path)-suffixLen]

	rootCategoryID, err := strconv.Atoi(rootCategoryIDStr)
	if err != nil || rootCategoryID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect category id")
		return
	}

	rootCategory, err := getCategoryByID(rootCategoryID)
	if err != nil || rootCategory.ParentID != 0 {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	items := itemsPool.Get()
	if itemID > 0 && createdAt > 0 {
		// paging
		err = dbx.Select(&items,
			"SELECT id, seller_id, name, price, image_name, category_id, created_at FROM `items` WHERE `status` = ? AND `parent_category_id` = ? AND (`created_at` < ?  OR (`created_at` = ? AND `id` < ?)) ORDER BY `created_at` DESC LIMIT ?",
			ItemStatusOnSale,
			rootCategoryID,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		// 1st page
		err = dbx.Select(&items,
			"SELECT id, seller_id, name, price, image_name, category_id, created_at FROM `items` WHERE `status` = ? AND `parent_category_id` = ? ORDER BY created_at DESC LIMIT ?",
			ItemStatusOnSale,
			rootCategoryID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	itemSimples := make(ItemSimples, len(items))

	for i := range items {
		seller, err := getUserSimpleByID(items[i].SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			return
		}
		category, err := getCategoryByID(items[i].CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples[i] = &ItemSimple{
			ID:         items[i].ID,
			SellerID:   items[i].SellerID,
			Seller:     &seller,
			Status:     ItemStatusOnSale,
			Name:       items[i].Name,
			Price:      items[i].Price,
			ImageURL:   getImageURL(items[i].ImageName),
			CategoryID: items[i].CategoryID,
			Category:   &category,
			CreatedAt:  items[i].CreatedAt.Unix(),
		}
	}

	itemsPool.Put(items)

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rni := resNewItems{
		RootCategoryID:   rootCategory.ID,
		RootCategoryName: rootCategory.CategoryName,
		Items:            &itemSimples,
		HasNext:          hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	err = gojay.NewEncoder(w).Encode(&rni)
	if err != nil {
		log.Print(err)
	}

}

func getUserItems(w http.ResponseWriter, r *http.Request) {
	// /users/:user_id.json
	prefixLen := len("/users/")
	suffixLen := len(".json")
	userIDStr := r.URL.Path[prefixLen : len(r.URL.Path)-suffixLen]

	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil || userID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect user id")
		return
	}

	userSimple, err := getUserSimpleByID(userID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	if userSimple.NumSellItems == 0 {
		rui := resUserItems{
			User:    &userSimple,
			Items:   &ItemSimples{},
			HasNext: false,
		}
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		err := gojay.NewEncoder(w).Encode(&rui)
		if err != nil {
			log.Print(err)
		}
		return
	}

	items := itemsPool.Get()
	if itemID > 0 && createdAt > 0 {
		// paging
		err := dbx.Select(&items,
			"SELECT id, status, name, price, image_name, category_id, created_at FROM `items` WHERE `seller_id` = ? AND (`created_at` < ?  OR (`created_at` = ? AND `id` < ?)) ORDER BY `created_at` DESC LIMIT ?",
			userSimple.ID,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	} else {
		// 1st page
		err := dbx.Select(&items,
			"SELECT id, status, name, price, image_name, category_id, created_at FROM `items` WHERE `seller_id` = ? ORDER BY `created_at` DESC LIMIT ?",
			userSimple.ID,
			ItemsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			return
		}
	}

	itemSimples := make(ItemSimples, len(items))
	for i := range items {
		category, err := getCategoryByID(items[i].CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			return
		}
		itemSimples[i] = &ItemSimple{
			ID:         items[i].ID,
			SellerID:   userSimple.ID,
			Seller:     &userSimple,
			Status:     items[i].Status,
			Name:       items[i].Name,
			Price:      items[i].Price,
			ImageURL:   getImageURL(items[i].ImageName),
			CategoryID: items[i].CategoryID,
			Category:   &category,
			CreatedAt:  items[i].CreatedAt.Unix(),
		}
	}

	itemsPool.Put(items)

	hasNext := false
	if len(itemSimples) > ItemsPerPage {
		hasNext = true
		itemSimples = itemSimples[0:ItemsPerPage]
	}

	rui := resUserItems{
		User:    &userSimple,
		Items:   &itemSimples,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	err = gojay.NewEncoder(w).Encode(&rui)
	if err != nil {
		log.Print(err)
	}
}

type TransactionAdditions struct {
	ItemID                    int64  `json:"item_id" db:"item_id"`
	TransactionEvidenceID     int64  `json:"id" db:"id"`
	TransactionEvidenceStatus string `json:"status" db:"status"`
	ShippingStatus            string `db:"-"`
}
type APIShippingStatus struct {
	Status string
	err    error
}

// ""が返ってきたときにAPIをたたく必要あり
func checkShippingStatusWithCacheShipping(transactionEvidenceID int64, status string) string {
	if status != ShippingsStatusShipping {
		return status
	}
	return checkShippingStatusWithCacheAny(transactionEvidenceID)
}

// ""が返ってきたときにAPIをたたく必要あり
func checkShippingStatusWithCacheAny(transactionEvidenceID int64) string {
	_, isDone := doneTransactionEvidences[transactionEvidenceID]
	if isDone {
		return ShippingsStatusDone
	}
	return ""
}

func checkShippingStatusWithAPI(transactionEvidenceID int64, reserveID string) (string, error) {
	ssr, err := APIShipmentStatus(shipmentServiceURL, &APIShipmentStatusReq{
		ReserveID: reserveID,
	})
	if err != nil {
		return "", err
	}

	if ssr.Status == ShippingsStatusDone {
		doneTransactionEvidences[transactionEvidenceID] = struct{}{}
	}
	return ssr.Status, nil
}

func requestShippingStatus(transactionEvidenceID int64, reserveID string, m *APIShippingStatusMap, wg *sync.WaitGroup) {
	defer wg.Done()
	status, err := checkShippingStatusWithAPI(transactionEvidenceID, reserveID)
	if err != nil {
		for i := 0; i < 3; i++ {
			status, err = checkShippingStatusWithAPI(transactionEvidenceID, reserveID)
			if err == nil {
				break
			}
		}
	}
	m.Store(transactionEvidenceID, APIShippingStatus{
		Status: status,
		err:    err,
	})
}

type APIShippingStatusMap struct {
	s sync.Map
}

func NewAPIShippingStatusMap() APIShippingStatusMap {
	return APIShippingStatusMap{}
}
func (s *APIShippingStatusMap) Store(key int64, value APIShippingStatus) {
	s.s.Store(key, value)
}
func (s *APIShippingStatusMap) Load(key int64) (APIShippingStatus, bool) {
	v, ok := s.s.Load(key)
	return v.(APIShippingStatus), ok
}

type ShippingSimple struct {
	TransactionEvidenceID int64  `json:"transaction_evidence_id" db:"transaction_evidence_id"`
	Status                string `json:"status" db:"status"`
}

func getShippingStatuses(tx *sqlx.Tx, w http.ResponseWriter, transactionEvidenceIDs []int64) ([]ShippingSimple, bool) {
	query, args, err := sqlx.In("SELECT transaction_evidence_id, status FROM `shippings` WHERE `transaction_evidence_id` IN (?)", transactionEvidenceIDs)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "query error")
		tx.Rollback()
		return []ShippingSimple{}, true
	}
	query = dbx.Rebind(query)

	shippings := make([]ShippingSimple, 0, len(transactionEvidenceIDs))
	err = sqlx.Select(tx, &shippings, query, args...)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "shipping not found")
		tx.Rollback()
		return shippings, true
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return shippings, true
	}

	return shippings, false

	/*
		resMap := NewAPIShippingStatusMap()
		ssMap = make(map[int64]string, len(shippings))
		wg := sync.WaitGroup{}

		for _, s := range shippings {
			status := checkShippingStatusWithCacheShipping(s.TransactionEvidenceID, s.Status)
			if status == "" {
				wg.Add(1)
				go requestShippingStatus(s.TransactionEvidenceID, s.ReserveID, &resMap, &wg)
			} else {
				ssMap[s.TransactionEvidenceID] = status
			}
		}

		wg.Wait()

		for _, s := range shippings {
			_, exists := ssMap[s.TransactionEvidenceID]
			if !exists {
				val, _ := resMap.Load(s.TransactionEvidenceID)
				if val.err != nil {
					log.Print(val.err)
					outputErrorMsg(w, http.StatusGatewayTimeout, "failed to request to shipment service")
					tx.Rollback()
					return ssMap, true
				}
				ssMap[s.TransactionEvidenceID] = val.Status
			}
		}
	*/
}

func getTransactionAdditions(tx *sqlx.Tx, w http.ResponseWriter, itemIDs []int64) ([]TransactionAdditions, bool) {
	query, args, err := sqlx.In("SELECT id, status, item_id FROM `transaction_evidences` WHERE `item_id` IN (?)", itemIDs)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "query error")
		tx.Rollback()
		return []TransactionAdditions{}, true
	}
	query = dbx.Rebind(query)

	tas := make([]TransactionAdditions, 0, len(itemIDs))
	err = sqlx.Select(tx, &tas, query, args...)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return tas, true
	}
	// It's able to ignore ErrNoRows
	if len(tas) <= 0 {
		return tas, false
	}

	ids := make([]int64, 0, len(tas))
	for i := range tas {
		if tas[i].TransactionEvidenceStatus != TransactionEvidenceStatusDone {
			ids = append(ids, tas[i].TransactionEvidenceID)
		}
	}

	shippings := make([]ShippingSimple, 0)
	if len(ids) > 0 {
		var hadErr bool
		shippings, hadErr = getShippingStatuses(tx, w, ids)
		if hadErr {
			return tas, true
		}
	}

	for i := range tas {
		if tas[i].TransactionEvidenceStatus != TransactionEvidenceStatusDone {
			for j := range shippings {
				if tas[i].TransactionEvidenceID == shippings[j].TransactionEvidenceID {
					tas[i].ShippingStatus = shippings[j].Status
					break
				}
			}
		} else {
			tas[i].ShippingStatus = ShippingsStatusDone
		}
	}
	return tas, false
}

func getTransactions(w http.ResponseWriter, r *http.Request) {
	userId, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	query := r.URL.Query()
	itemIDStr := query.Get("item_id")
	var err error
	var itemID int64
	if itemIDStr != "" {
		itemID, err = strconv.ParseInt(itemIDStr, 10, 64)
		if err != nil || itemID <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "item_id param error")
			return
		}
	}

	createdAtStr := query.Get("created_at")
	var createdAt int64
	if createdAtStr != "" {
		createdAt, err = strconv.ParseInt(createdAtStr, 10, 64)
		if err != nil || createdAt <= 0 {
			outputErrorMsg(w, http.StatusBadRequest, "created_at param error")
			return
		}
	}

	tx := dbx.MustBegin()
	items := itemsTPool.Get()
	if itemID > 0 && createdAt > 0 {
		// paging
		err := tx.Select(&items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) AND (`created_at` < ?  OR (`created_at` = ? AND `id` < ?)) ORDER BY `created_at` DESC LIMIT ?",
			userId,
			userId,
			time.Unix(createdAt, 0),
			time.Unix(createdAt, 0),
			itemID,
			TransactionsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}
	} else {
		// 1st page
		err := tx.Select(&items,
			"SELECT * FROM `items` WHERE (`seller_id` = ? OR `buyer_id` = ?) ORDER BY `created_at` DESC LIMIT ?",
			userId,
			userId,
			TransactionsPerPage+1,
		)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusInternalServerError, "db error")
			tx.Rollback()
			return
		}
	}

	itemIds := make([]int64, len(items))
	for i := range items {
		if items[i].Status != ItemStatusOnSale {
			itemIds[i] = items[i].ID
		}
	}

	transactionAdditions, hadErr := getTransactionAdditions(tx, w, itemIds)
	if hadErr {
		return
	}

	itemDetails := make(ItemDetails, len(items))
	for i := range items {
		seller, err := getUserSimpleByID(items[i].SellerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "seller not found")
			tx.Rollback()
			return
		}
		category, err := getCategoryByID(items[i].CategoryID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "category not found")
			tx.Rollback()
			return
		}

		itemDetail := ItemDetail{
			ID:       items[i].ID,
			SellerID: items[i].SellerID,
			Seller:   &seller,
			// BuyerID
			// Buyer
			Status:      items[i].Status,
			Name:        items[i].Name,
			Price:       items[i].Price,
			Description: items[i].Description,
			ImageURL:    getImageURL(items[i].ImageName),
			CategoryID:  items[i].CategoryID,
			// TransactionEvidenceID
			// TransactionEvidenceStatus
			// ShippingStatus
			Category:  &category,
			CreatedAt: items[i].CreatedAt.Unix(),
		}

		if items[i].BuyerID != 0 {
			buyer, err := getUserSimpleByID(items[i].BuyerID)
			if err != nil {
				outputErrorMsg(w, http.StatusNotFound, "buyer not found")
				tx.Rollback()
				return
			}
			itemDetail.BuyerID = items[i].BuyerID
			itemDetail.Buyer = &buyer
		}

		for j := range transactionAdditions {
			if items[i].ID == transactionAdditions[j].ItemID {
				itemDetail.TransactionEvidenceID = transactionAdditions[j].TransactionEvidenceID
				itemDetail.TransactionEvidenceStatus = transactionAdditions[j].TransactionEvidenceStatus
				itemDetail.ShippingStatus = transactionAdditions[j].ShippingStatus
			}
		}

		itemDetails[i] = &itemDetail
	}
	tx.Commit()

	itemsTPool.Put(items)

	hasNext := false
	if len(itemDetails) > TransactionsPerPage {
		hasNext = true
		itemDetails = itemDetails[0:TransactionsPerPage]
	}

	rts := resTransactions{
		Items:   &itemDetails,
		HasNext: hasNext,
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	err = gojay.NewEncoder(w).Encode(&rts)
	if err != nil {
		log.Print(err)
	}
}

type ItemEPool struct {
	p sync.Pool
}

func NewItemEPool() ItemEPool {
	return ItemEPool{
		p: sync.Pool{
			New: func() interface{} {
				return &ItemE{}
			},
		},
	}
}
func (p *ItemEPool) Get() *ItemE {
	return p.p.Get().(*ItemE)
}
func (p *ItemEPool) Put(i *ItemE) {
	p.p.Put(i)
}

type ItemE struct {
	Item                      Item           `db:"i"`
	TransactionEvidenceID     sql.NullInt64  `db:"te_id"`
	TransactionEvidenceStatus sql.NullString `db:"te_status"`
	ShippingStatus            sql.NullString `db:"s_status"`
}

func getItem(w http.ResponseWriter, r *http.Request) {
	// /items/:item_id.json
	prefixLen := len("/items/")
	suffixLen := len(".json")
	itemIDStr := r.URL.Path[prefixLen : len(r.URL.Path)-suffixLen]

	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil || itemID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect item id")
		return
	}

	userID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	itemE := itemEPool.Get()
	rows := "i.seller_id AS `i.seller_id`, i.buyer_id AS `i.buyer_id`, i.status AS `i.status`, i.name AS `i.name`, i.price AS `i.price`, i.description AS `i.description`, i.image_name AS `i.image_name`, i.category_id AS `i.category_id`, te.id AS `te_id`, te.status AS `te_status`, s.status AS `s_status`"
	err = dbx.Get(itemE, "SELECT "+rows+" FROM `items` AS `i` LEFT JOIN `transaction_evidences` AS `te` ON `te`.`item_id` = `i`.`id` LEFT JOIN `shippings` AS `s` ON `s`.`transaction_evidence_id` = `te`.`id` WHERE `i`.`id` = ?", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}
	item := itemE.Item

	category, err := getCategoryByID(item.CategoryID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "category not found")
		return
	}

	seller, err := getUserSimpleByID(item.SellerID)
	if err != nil {
		outputErrorMsg(w, http.StatusNotFound, "seller not found")
		return
	}

	itemDetail := ItemDetail{
		ID:       itemID,
		SellerID: item.SellerID,
		Seller:   &seller,
		// BuyerID
		// Buyer
		Status:      item.Status,
		Name:        item.Name,
		Price:       item.Price,
		Description: item.Description,
		ImageURL:    getImageURL(item.ImageName),
		CategoryID:  item.CategoryID,
		// TransactionEvidenceID
		// TransactionEvidenceStatus
		// ShippingStatus
		Category:  &category,
		CreatedAt: item.CreatedAt.Unix(),
	}

	if (userID == item.SellerID || userID == item.BuyerID) && item.BuyerID != 0 {
		buyer, err := getUserSimpleByID(item.BuyerID)
		if err != nil {
			outputErrorMsg(w, http.StatusNotFound, "buyer not found")
			return
		}
		itemDetail.BuyerID = item.BuyerID
		itemDetail.Buyer = &buyer

		if itemE.TransactionEvidenceID.Valid && itemE.TransactionEvidenceID.Int64 > 0 {
			itemDetail.TransactionEvidenceID = itemE.TransactionEvidenceID.Int64
			itemDetail.TransactionEvidenceStatus = itemE.TransactionEvidenceStatus.String
			if !itemE.ShippingStatus.Valid {
				log.Print("db error (shippingStatus is empty)")
				outputErrorMsg(w, http.StatusInternalServerError, "db error (shippingStatus is empty)")
				return
			}
			itemDetail.ShippingStatus = itemE.ShippingStatus.String
		}
	}

	itemEPool.Put(itemE)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	err = gojay.NewEncoder(w).Encode(&itemDetail)
	if err != nil {
		log.Print(err)
	}
}

func postItemEdit(w http.ResponseWriter, r *http.Request) {
	rie := reqItemEdit{}
	err := json.NewDecoder(r.Body).Decode(&rie)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := rie.CSRFToken
	itemID := rie.ItemID
	price := rie.ItemPrice

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)
		return
	}

	sellerID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()

	targetItem := Item{}
	err = tx.Get(&targetItem, "SELECT seller_id, status, created_at FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.SellerID != sellerID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		tx.Rollback()
		return
	}

	if targetItem.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "販売中の商品以外編集できません")
		tx.Rollback()
		return
	}

	now := time.Now()
	_, err = tx.Exec("UPDATE `items` SET `price` = ?, `updated_at` = ? WHERE `id` = ?",
		price,
		now,
		itemID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        itemID,
		ItemPrice:     price,
		ItemCreatedAt: targetItem.CreatedAt.Unix(),
		ItemUpdatedAt: now.Unix(),
	})
}

type TEWithS struct {
	SellerID  int64  `db:"seller_id"`
	Status    string `db:"status"`
	ImgBinary []byte `db:"img_binary"`
}

func getQRCode(w http.ResponseWriter, r *http.Request) {
	transactionEvidenceIDStr := pat.Param(r, "transaction_evidence_id")
	transactionEvidenceID, err := strconv.ParseInt(transactionEvidenceIDStr, 10, 64)
	if err != nil || transactionEvidenceID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "incorrect transaction_evidence id")
		return
	}

	sellerID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	teWithS := TEWithS{}
	err = dbx.Get(&teWithS, "SELECT `te`.`seller_id` AS `seller_id`, `s`.`status` AS `status`, `s`.`img_binary` AS `img_binary` FROM `transaction_evidences` AS `te` JOIN `shippings` AS `s` ON `te`.`id` = `s`.`transaction_evidence_id` WHERE `te`.`id` = ?", transactionEvidenceID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences or shippings not found")
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if teWithS.SellerID != sellerID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	if teWithS.Status != ShippingsStatusWaitPickup && teWithS.Status != ShippingsStatusShipping {
		outputErrorMsg(w, http.StatusForbidden, "qrcode not available")
		return
	}

	if len(teWithS.ImgBinary) == 0 {
		outputErrorMsg(w, http.StatusInternalServerError, "empty qrcode image")
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "max-age=31536000, public")
	w.Write(teWithS.ImgBinary)
}

type BuyingMutex struct {
	sync.RWMutex
	// nilのとき処理中、falseのとき無効なItem、trueのとき売却済み
	Result *bool
	Cond   *sync.Cond
	// すでに次のを呼び出し済み
	SentSignal bool
}
type BuyingMutexMap struct {
	s sync.Map
}

func NewBuyingMutexMap() BuyingMutexMap {
	return BuyingMutexMap{}
}
func (s *BuyingMutexMap) Add(key int64, cond *sync.Cond) {
	s.s.Store(key, &BuyingMutex{
		Result:     nil,
		Cond:       cond,
		SentSignal: false,
	})
}
func (s *BuyingMutexMap) SetResult(key int64, result *bool) {
	val, _ := s.s.Load(key)
	v := val.(*BuyingMutex)

	v.Lock()
	defer v.Unlock()

	v.Result = result
}
func (s *BuyingMutexMap) SetSuccess(key int64) {
	res := true
	s.SetResult(key, &res)

	val, _ := s.Load(key)
	val.Cond.Broadcast()
}
func (s *BuyingMutexMap) SetFailure(key int64) {
	s.SetResult(key, nil)

	val, _ := s.Load(key)
	val.Lock()
	defer val.Unlock()

	val.Cond.Signal()
	val.SentSignal = true
}
func (s *BuyingMutexMap) ReceivedFailure(key int64) {
	val, _ := s.Load(key)
	val.Lock()
	defer val.Unlock()

	val.SentSignal = false
}
func (s *BuyingMutexMap) SetInvalid(key int64) {
	res := false
	s.SetResult(key, &res)

	val, _ := s.Load(key)
	val.Cond.Broadcast()
}
func (s *BuyingMutexMap) Load(key int64) (*BuyingMutex, bool) {
	val, exists := s.s.Load(key)
	if exists {
		return val.(*BuyingMutex), exists
	}
	return nil, exists
}
func (s *BuyingMutexMap) Delete(key int64) {
	s.s.Delete(key)
}

func postBuy(w http.ResponseWriter, r *http.Request) {
	rb := reqBuy{}

	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}
	itemID := rb.ItemID

	if rb.CSRFToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	buyer, errCode, errMsg := getUser(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	if mutex, isRunning := buyingMutexMap.Load(itemID); isRunning {
		if mutex.Result == nil {
			if !mutex.SentSignal {
				mutex.Cond.L.Lock()
				defer mutex.Cond.L.Unlock()
				mutex.Cond.Wait()
			}
			buyingMutexMap.ReceivedFailure(itemID)
		}
		if mutex.Result != nil {
			if *mutex.Result == true {
				outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
				return
			}
			outputErrorMsg(w, http.StatusNotFound, "item or item seller not found")
			return
		}
	} else {
		l := new(sync.Mutex)
		c := sync.NewCond(l)
		buyingMutexMap.Add(itemID, c)
	}

	tx := dbx.MustBegin()

	item := Item{}
	err = tx.Get(&item, "SELECT * FROM `items` WHERE `items`.`id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item or item seller not found")
		tx.Rollback()
		buyingMutexMap.SetInvalid(itemID)
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	if item.Status != ItemStatusOnSale {
		outputErrorMsg(w, http.StatusForbidden, "item is not for sale")
		tx.Rollback()
		buyingMutexMap.SetSuccess(itemID)
		return
	}

	if item.SellerID == buyer.ID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品は買えません")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	category, err := getCategoryByID(item.CategoryID)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "category id error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	if rb.Token == "" {
		outputErrorMsg(w, http.StatusBadRequest, "カード情報に誤りがあります")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	// addressとnameは書き変わらないのでRLockでとる
	sellar, ok := usersCache.Get(item.SellerID)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "sellar not found")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	result, err := tx.Exec("INSERT INTO `transaction_evidences` (`seller_id`, `buyer_id`, `status`, `item_id`) VALUES (?, ?, ?, ?)",
		item.SellerID,
		buyer.ID,
		TransactionEvidenceStatusWaitShipping,
		item.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	transactionEvidenceID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `buyer_id` = ?, `status` = ?, `updated_at` = ? WHERE `id` = ?",
		buyer.ID,
		ItemStatusTrading,
		time.Now(),
		item.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	// TODO: 並列
	var scr *APIShipmentCreateRes
	var pstr *APIPaymentServiceTokenRes

	type ScrStruct struct {
		scr *APIShipmentCreateRes
		err error
	}
	type PstrStruct struct {
		pstr *APIPaymentServiceTokenRes
		err  error
	}
	scrChan := make(chan ScrStruct)
	pstrChan := make(chan PstrStruct)
	go func() {
		scr, err := APIShipmentCreate(shipmentServiceURL, &APIShipmentCreateReq{
			ToAddress:   buyer.Address,
			ToName:      buyer.AccountName,
			FromAddress: sellar.Address,
			FromName:    sellar.AccountName,
		})
		str := ScrStruct{
			scr: scr,
			err: err,
		}
		scrChan <- str
	}()
	go func() {
		pstr, err := APIPaymentToken(paymentServiceURL, &APIPaymentServiceTokenReq{
			ShopID: PaymentServiceIsucariShopID,
			Token:  rb.Token,
			APIKey: PaymentServiceIsucariAPIKey,
			Price:  item.Price,
		})
		str := PstrStruct{
			pstr: pstr,
			err:  err,
		}
		pstrChan <- str
	}()

	for i := 0; i < 2; i++ {
		select {
		case str := <-scrChan:
			scr = str.scr
			err = str.err
			if err != nil {
				log.Print(err)
				outputErrorMsg(w, http.StatusGatewayTimeout, "failed to request to shipment service")
				tx.Rollback()
				buyingMutexMap.SetFailure(itemID)
				return
			}
		case str := <-pstrChan:
			pstr = str.pstr
			err = str.err
			if err != nil {
				log.Print(err)

				outputErrorMsg(w, http.StatusGatewayTimeout, "payment service is failed")
				tx.Rollback()
				buyingMutexMap.SetFailure(itemID)
				return
			}
		}
	}

	if pstr.Status == "invalid" {
		outputErrorMsg(w, http.StatusBadRequest, "カード情報に誤りがあります")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	if pstr.Status == "fail" {
		outputErrorMsg(w, http.StatusBadRequest, "カードの残高が足りません")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	if pstr.Status != "ok" {
		outputErrorMsg(w, http.StatusBadRequest, "想定外のエラー")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	_, err = tx.Exec("INSERT INTO `shippings` (`transaction_evidence_id`, `status`, `reserve_id`, `reserve_time`, `img_binary`) VALUES (?,?,?,?,?)",
		transactionEvidenceID,
		ShippingsStatusInitial,
		item.Name,
		item.ID,
		scr.ReserveID,
		scr.ReserveTime,
		buyer.Address,
		buyer.AccountName,
		sellar.Address,
		sellar.AccountName,
		"",
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		buyingMutexMap.SetFailure(itemID)
		return
	}

	tx.Commit()
	buyingMutexMap.SetSuccess(itemID)

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidenceID})
}

type TeShip struct {
	TeID      int64  `db:"te_id"`
	SellerID  int64  `db:"seller_id"`
	TeStatus  string `db:"te_status"`
	ReserveID string `db:"reserve_id"`
}

func postShip(w http.ResponseWriter, r *http.Request) {
	reqps := reqPostShip{}

	err := json.NewDecoder(r.Body).Decode(&reqps)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqps.CSRFToken
	itemID := reqps.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	sellerID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()

	teShip := TeShip{}
	err = tx.Get(&teShip, "SELECT `te`.`id` AS `te_id`, `te`.`seller_id` AS `seller_id`, `te`.`status` AS `te_status`, `s`.`reserve_id` AS `reserve_id` FROM `transaction_evidences` as `te` JOIN `shippings` AS `s` ON `te`.`id` = `s`.`transaction_evidence_id` WHERE `te`.`item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences or shippings not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if teShip.SellerID != sellerID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		tx.Rollback()
		return
	}

	if teShip.TeStatus != TransactionEvidenceStatusWaitShipping {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	img, err := APIShipmentRequest(shipmentServiceURL, &APIShipmentRequestReq{
		ReserveID: teShip.ReserveID,
	})
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusGatewayTimeout, "failed to request to shipment service")
		tx.Rollback()

		return
	}

	_, err = tx.Exec("UPDATE `shippings` SET `status` = ?, `img_binary` = ?, `updated_at` = ? WHERE `transaction_evidence_id` = ?",
		ShippingsStatusWaitPickup,
		img,
		time.Now(),
		teShip.TeID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	rps := resPostShip{
		Path:      "/transactions/" + strconv.FormatInt(teShip.TeID, 10) + ".png",
		ReserveID: teShip.ReserveID,
	}
	json.NewEncoder(w).Encode(rps)
}

type TeShipDone struct {
	TeID      int64  `db:"te_id"`
	SellerID  int64  `db:"seller_id"`
	SStatus   string `db:"s_status"`
	ReserveID string `db:"reserve_id"`
}

func postShipDone(w http.ResponseWriter, r *http.Request) {
	reqpsd := reqPostShipDone{}

	err := json.NewDecoder(r.Body).Decode(&reqpsd)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqpsd.CSRFToken
	itemID := reqpsd.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	sellerID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()

	teShipDone := TeShipDone{}
	err = tx.Get(&teShipDone, "SELECT `te`.`id` AS `te_id`, `te`.`seller_id` AS `seller_id`, `s`.`status` AS `s_status`, `s`.`reserve_id` AS `reserve_id` FROM `transaction_evidences` as `te` JOIN `shippings` AS `s` ON `te`.`id` = `s`.`transaction_evidence_id` WHERE `te`.`item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences or shippings not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if teShipDone.SellerID != sellerID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		tx.Rollback()
		return
	}

	if teShipDone.SStatus != ShippingsStatusWaitPickup {
		outputErrorMsg(w, http.StatusForbidden, "集荷予約されてません")
		tx.Rollback()
		return
	}

	status := checkShippingStatusWithCacheAny(teShipDone.TeID)
	if status == "" {
		status, err = checkShippingStatusWithAPI(teShipDone.TeID, teShipDone.ReserveID)
		if err != nil {
			log.Print(err)
			outputErrorMsg(w, http.StatusGatewayTimeout, "failed to request to shipment service")
			tx.Rollback()

			return
		}
	}

	if !(status == ShippingsStatusShipping || status == ShippingsStatusDone) {
		outputErrorMsg(w, http.StatusForbidden, "shipment service側で配送中か配送完了になっていません")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `shippings` AS `s` JOIN `transaction_evidences` AS `te` ON `s`.`transaction_evidence_id` = `te`.`id` SET `s`.`status` = ?, `s`.`updated_at` = ?, `te`.`status` = ?, `te`.`updated_at` = ? WHERE `s`.`transaction_evidence_id` = ?",
		status,
		time.Now(),
		TransactionEvidenceStatusWaitDone,
		time.Now(),
		teShipDone.TeID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: teShipDone.TeID})
}

func postComplete(w http.ResponseWriter, r *http.Request) {
	reqpc := reqPostComplete{}

	err := json.NewDecoder(r.Body).Decode(&reqpc)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := reqpc.CSRFToken
	itemID := reqpc.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")

		return
	}

	buyerID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()
	transactionEvidence := TransactionEvidence{}
	err = tx.Get(&transactionEvidence, "SELECT id, buyer_id, status FROM `transaction_evidences` WHERE `item_id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "transaction_evidences not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if transactionEvidence.BuyerID != buyerID {
		outputErrorMsg(w, http.StatusForbidden, "権限がありません")
		return
	}

	if transactionEvidence.Status != TransactionEvidenceStatusWaitDone {
		outputErrorMsg(w, http.StatusForbidden, "準備ができていません")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `shippings` AS `s` JOIN `transaction_evidences` AS `te` ON `s`.`transaction_evidence_id` = `te`.`id` JOIN `items` AS `i` ON `i`.`id` = `te`.`item_id` SET `s`.`status` = ?, `s`.`updated_at` = ?, `te`.`status` = ?, `te`.`updated_at` = ?, `i`.`status` = ?, `i`.`updated_at` = ? WHERE `s`.`transaction_evidence_id` = ?",
		ShippingsStatusDone,
		time.Now(),
		TransactionEvidenceStatusDone,
		time.Now(),
		ItemStatusSoldOut,
		time.Now(),
		transactionEvidence.ID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resBuy{TransactionEvidenceID: transactionEvidence.ID})
}

func postSell(w http.ResponseWriter, r *http.Request) {
	csrfToken := r.FormValue("csrf_token")
	name := r.FormValue("name")
	description := r.FormValue("description")
	priceStr := r.FormValue("price")
	categoryIDStr := r.FormValue("category_id")

	if name == "" || description == "" || priceStr == "" || categoryIDStr == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")
		return
	}

	f, header, err := r.FormFile("image")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusBadRequest, "image error")
		return
	}
	defer f.Close()

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	categoryID, err := strconv.Atoi(categoryIDStr)
	if err != nil || categoryID <= 0 {
		outputErrorMsg(w, http.StatusBadRequest, "category id error")
		return
	}

	price, err := strconv.Atoi(priceStr)
	if err != nil || price == 0 {
		outputErrorMsg(w, http.StatusBadRequest, "price error")
		return
	}

	if price < ItemMinPrice || price > ItemMaxPrice {
		outputErrorMsg(w, http.StatusBadRequest, ItemPriceErrMsg)
		return
	}

	category, err := getCategoryByID(categoryID)
	if err != nil || category.ParentID == 0 {
		//log.Print(categoryID, category)
		outputErrorMsg(w, http.StatusBadRequest, "Incorrect category ID")
		return
	}

	userID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	img, err := ioutil.ReadAll(f)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "image error")
		return
	}

	ext := filepath.Ext(header.Filename)

	if !(ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif") {
		outputErrorMsg(w, http.StatusBadRequest, "unsupported image format error")
		return
	}

	if ext == ".jpeg" {
		ext = ".jpg"
	}

	imgName := secureRandomStr(16) + ext
	err = ioutil.WriteFile("../public/upload/"+imgName, img, 0644)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "Saving image failed")
		return
	}

	usersCache.Lock(userID)
	defer usersCache.Unlock(userID)

	seller, ok := usersCache.GetWithoutLock(userID)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		return
	}

	tx := dbx.MustBegin()

	result, err := tx.Exec("INSERT INTO `items` (`seller_id`, `status`, `name`, `price`, `description`,`image_name`,`category_id`,`parent_category_id`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		seller.ID,
		ItemStatusOnSale,
		name,
		price,
		description,
		imgName,
		category.ID,
		category.ParentID,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	itemID, err := result.LastInsertId()
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	seller.NumSellItems++
	seller.LastBump = time.Now()
	usersCache.SetWithoutLock(seller.ID, seller)
	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(resSell{ID: itemID})
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return hex.EncodeToString(k)
}

func postBump(w http.ResponseWriter, r *http.Request) {
	rb := reqBump{}
	err := json.NewDecoder(r.Body).Decode(&rb)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	csrfToken := rb.CSRFToken
	itemID := rb.ItemID

	if csrfToken != getCSRFToken(r) {
		outputErrorMsg(w, http.StatusUnprocessableEntity, "csrf token error")
		return
	}

	userID, errCode, errMsg := getUserID(r)
	if errMsg != "" {
		outputErrorMsg(w, errCode, errMsg)
		return
	}

	tx := dbx.MustBegin()

	targetItem := Item{}
	err = tx.Get(&targetItem, "SELECT seller_id, price FROM `items` WHERE `id` = ? FOR UPDATE", itemID)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusNotFound, "item not found")
		tx.Rollback()
		return
	}
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		tx.Rollback()
		return
	}

	if targetItem.SellerID != userID {
		outputErrorMsg(w, http.StatusForbidden, "自分の商品以外は編集できません")
		tx.Rollback()
		return
	}

	usersCache.Lock(userID)
	defer usersCache.Unlock(userID)

	seller, ok := usersCache.GetWithoutLock(userID)
	if !ok {
		outputErrorMsg(w, http.StatusNotFound, "user not found")
		tx.Rollback()
		return
	}

	now := time.Now()
	// last_bump + 3s > now
	if seller.LastBump.Add(BumpChargeSeconds).After(now) {
		outputErrorMsg(w, http.StatusForbidden, "Bump not allowed")
		tx.Rollback()
		return
	}

	_, err = tx.Exec("UPDATE `items` SET `created_at`=?, `updated_at`=? WHERE id=?",
		now,
		now,
		itemID,
	)
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	seller.LastBump = now
	usersCache.SetWithoutLock(seller.ID, seller)

	tx.Commit()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(&resItemEdit{
		ItemID:        itemID,
		ItemPrice:     targetItem.Price,
		ItemCreatedAt: now.Round(time.Second).Unix(),
		ItemUpdatedAt: now.Round(time.Second).Unix(),
	})
}

func getSettings(w http.ResponseWriter, r *http.Request) {
	csrfToken := getCSRFToken(r)

	user, _, errMsg := getUser(r)

	ress := resSetting{}
	ress.CSRFToken = csrfToken
	if errMsg == "" {
		ress.User = &user
	}

	ress.PaymentServiceURL = paymentServiceURL

	ress.Categories = categories

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(ress)
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	res, statusCode := APIAuthCheck(&r.Body)
	if statusCode == http.StatusUnauthorized {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	if statusCode != http.StatusOK {
		outputErrorMsg(w, http.StatusInternalServerError, "internal request failed")
		return
	}

	user, ok := usersCache.GetByAccountName(res.AccountName)
	if !ok {
		outputErrorMsg(w, http.StatusInternalServerError, "user not found by account_name")
		return
	}

	//rl := reqLogin{}
	//err := json.NewDecoder(r.Body).Decode(&rl)
	//if err != nil {
	//	outputErrorMsg(w, http.StatusBadRequest, "json decode error")
	//	return
	//}
	//
	//accountName := rl.AccountName
	//password := rl.Password
	//
	//if accountName == "" || password == "" {
	//	outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")
	//
	//	return
	//}
	//
	//u := User{}
	//err = dbx.Get(&u, "SELECT * FROM `users` WHERE `account_name` = ?", accountName)
	//if err == sql.ErrNoRows {
	//	outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
	//	return
	//}
	//if err != nil {
	//	log.Print(err)
	//
	//	outputErrorMsg(w, http.StatusInternalServerError, "db error")
	//	return
	//}
	//
	//err = bcrypt.CompareHashAndPassword(u.HashedPassword, []byte(password))
	//if err == bcrypt.ErrMismatchedHashAndPassword {
	//	outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
	//	return
	//}
	//if err != nil {
	//	log.Print(err)
	//
	//	outputErrorMsg(w, http.StatusInternalServerError, "crypt error")
	//	return
	//}

	setSession(w, SessionData{
		UserID:    user.ID,
		CsrfToken: secureRandomStr(20),
	})

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(user)
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	rr := reqRegister{}
	err := json.NewDecoder(r.Body).Decode(&rr)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rr.AccountName
	address := rr.Address
	password := rr.Password

	if accountName == "" || password == "" || address == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "error")
		return
	}

	// ログインのためにDBに入れる
	result, err := dbx.Exec("INSERT INTO `users` (`account_name`, `hashed_password`, `address`) VALUES (?, ?, ?)",
		accountName,
		hashedPassword,
		address,
	)
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	userID, err := result.LastInsertId()

	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	u := User{
		ID:          userID,
		AccountName: accountName,
		Address:     address,
	}

	usersCache.Set(userID, u)

	setSession(w, SessionData{
		UserID:    u.ID,
		CsrfToken: secureRandomStr(20),
	})

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(u)
}

type Report struct {
	ID                 int64     `json:"id" db:"id"`
	SellerID           int64     `json:"seller_id" db:"seller_id"`
	BuyerID            int64     `json:"buyer_id" db:"buyer_id"`
	Status             string    `json:"status" db:"status"`
	ItemID             int64     `json:"item_id" db:"item_id"`
	ItemName           string    `json:"item_name" db:"item_name"`
	ItemPrice          int       `json:"item_price" db:"item_price"`
	ItemDescription    string    `json:"item_description" db:"item_description"`
	ItemCategoryID     int       `json:"item_category_id" db:"item_category_id"`
	ItemRootCategoryID int       `json:"item_root_category_id" db:"item_root_category_id"`
}

func getReports(w http.ResponseWriter, r *http.Request) {
	reports := make([]Report, 0)
	// ここいい感じにやる
	err := dbx.Select(&reports, "SELECT * FROM `transaction_evidences` WHERE `id` > 15007")
	if err != nil {
		log.Print(err)
		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	json.NewEncoder(w).Encode(reports)
}

func outputErrorMsg(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	w.WriteHeader(status)

	json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

func getImageURL(imageName string) string {
	return "/upload/" + imageName
}
