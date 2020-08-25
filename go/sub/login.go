// package main
package sub

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

type CacheMap struct {
	s sync.Map
}

func (s *CacheMap) Store(key string, value string) {
	s.s.Store(key, value)
}
func (s *CacheMap) Load(key string) (string, bool) {
	v, ok := s.s.Load(key)
	if ok {
		return v.(string), true
	}
	return "", false
}

var (
	dbx   *sqlx.DB
	cache CacheMap
)

func main() {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		"isucari",      // user
		"isucari",      // password
		"172.16.0.162", // host
		"3306",         // port
		"isucari",      // dbname
	)

	_dbx, err := sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("failed to connect to DB: %s.", err.Error())
	}
	dbx = _dbx
	defer dbx.Close()

	cache = CacheMap{}
	loadFileCache()
	// go pollDB(dbx)

	go func() {
		http.HandleFunc("/auth", auth)
		http.ListenAndServe(":8080", nil)
	}()

	quit := make(chan os.Signal)
	signal.Notify(quit, os.Interrupt)
	log.Print("waiting signal")
	<-quit
	flushCacheToFile()
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

type reqLogin struct {
	AccountName string `json:"account_name"`
	Password    string `json:"password"`
}

func auth(w http.ResponseWriter, r *http.Request) {
	rl := reqLogin{}
	err := json.NewDecoder(r.Body).Decode(&rl)
	if err != nil {
		outputErrorMsg(w, http.StatusBadRequest, "json decode error")
		return
	}

	accountName := rl.AccountName
	password := rl.Password

	if accountName == "" || password == "" {
		outputErrorMsg(w, http.StatusBadRequest, "all parameters are required")

		return
	}

	u := User{}
	err = dbx.Get(&u, "SELECT * FROM `users` WHERE `account_name` = ?", accountName)
	if err == sql.ErrNoRows {
		outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
		return
	}
	if err != nil {
		log.Print(err)

		outputErrorMsg(w, http.StatusInternalServerError, "db error")
		return
	}

	if cachedPass, ok := cache.Load(accountName); ok {
		isSame := subtle.ConstantTimeCompare([]byte(cachedPass), []byte(password)) == 1
		if !isSame {
			outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
			return
		}
	} else {
		err = bcrypt.CompareHashAndPassword(u.HashedPassword, []byte(password))
		if err == bcrypt.ErrMismatchedHashAndPassword {
			outputErrorMsg(w, http.StatusUnauthorized, "アカウント名かパスワードが間違えています")
			return
		}
		if err != nil {
			log.Print(err)

			outputErrorMsg(w, http.StatusInternalServerError, "crypt error")
			return
		}

		cache.Store(accountName, password)
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(u)
}

func outputErrorMsg(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	w.WriteHeader(status)

	json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: msg})
}

func pollDB(dbx *sqlx.DB) {
	for {
		err := dbx.Ping()
		if err != nil {
			log.Printf("Failed to ping DB: %s", err)
		}
		log.Println("ping pong")
		time.Sleep(time.Second)
	}
}

func loadFileCache() {
	if _, err := os.Stat("passwords.json"); os.IsNotExist(err) {
		log.Print("json file does not exist.")
		return
	}
	raw, err := ioutil.ReadFile("passwords.json")
	if err != nil {
		log.Fatal(err)
	}
	json.Unmarshal(raw, &cache)
}

func flushCacheToFile() {
	file, err := os.Create("passwords.json")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	bytes, _ := json.Marshal(&cache)
	_, err = file.Write(bytes)
	if err != nil {
		log.Fatal(err)
	}
	log.Print("cache flushed")
}
