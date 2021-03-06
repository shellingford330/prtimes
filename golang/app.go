package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "net/http/pprof"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	goji "goji.io"
	"goji.io/pat"
	"goji.io/pattern"
)

var (
	db               *sqlx.DB
	store            *gsm.MemcacheStore
	count            sync.Map
	postMime         sync.Map
	tplCache         sync.Map
	userCache        sync.Map
	userCommentCache sync.Map
	fmap             = template.FuncMap{"imageURL": imageURL}
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int       `db:"count"`
	Comments     []Comment
	User         User `db:"user"`
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User      `db:"user"`
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient := memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
		"UPDATE posts SET user_del_flg = 0",
		"UPDATE posts SET user_del_flg = 1 WHERE user_id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// ?????????Go????????????????????????????????????????????????????????????????????????OS??????????????????????????????????????????????????????
// ????????????PHP???escapeshellarg?????????????????????????????????
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	// openssl????????????????????????????????? (stdin)= ?????????????????????????????????
	out, err := exec.Command("/bin/bash", "-c", `printf "%s" `+escapeshellarg(src)+` | openssl dgst -sha512 | sed 's/^.*= //'`).Output()
	if err != nil {
		log.Print(err)
		return ""
	}

	return strings.TrimSuffix(string(out), "\n")
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	value, ok := userCache.Load(uid.(int))
	if !ok {
		return User{}
	}
	u := value.(User)

	// u := User{}

	// err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	// if err != nil {
	// 	return User{}
	// }

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func makePosts(results []Post, csrfToken string) ([]Post, error) {
	posts := make([]Post, 0, postsPerPage)

	for _, p := range results {
		// TODO: ?????????????????????
		// err := db.Get(&p.CommentCount, "SELECT COUNT(*) AS `count` FROM `comments` WHERE `post_id` = ?", p.ID)
		// if err != nil {
		// 	return nil, err
		// }
		value, ok := count.Load(p.ID)
		if !ok {
			return nil, errors.New("cannot load post's comment count")
		}
		p.CommentCount, ok = value.(int)
		if !ok {
			return nil, errors.New("failed to type assertion of comment count")
		}

		query := "SELECT `comment`, `user_id` FROM `comments` WHERE `post_id` = ? ORDER BY `created_at` DESC LIMIT 3"
		var comments []Comment
		err := db.Select(&comments, query, p.ID)
		if err != nil {
			return nil, err
		}

		for _, comment := range comments {
			value, ok := userCache.Load(comment.UserID)
			if !ok {
				return nil, errors.New("cannot load comment's user")
			}
			comment.User = value.(User)
		}

		// for i := 0; i < len(comments); i++ {
		// 	err := db.Get(&comments[i].User, "SELECT * FROM `users` WHERE `id` = ?", comments[i].UserID)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// }

		// reverse
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}

		p.Comments = comments

		// err = db.Get(&p.User, "SELECT * FROM `users` WHERE `id` = ?", p.UserID)
		// if err != nil {
		// 	return nil, err
		// }

		p.CSRFToken = csrfToken

		posts = append(posts, p)
		// if p.User.DelFlg == 0 {
		// 	posts = append(posts, p)
		// }
		// if len(posts) >= postsPerPage {
		// 	break
		// }
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "????????????????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????3?????????????????????????????????6??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ????????????????????????????????????????????????????????????????????????????????????????????????
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "???????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = int(uid)
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	userCache.Store(int(uid), User{
		ID:          int(uid),
		DelFlg:      0,
		Authority:   0,
		AccountName: accountName,
	})
	userCommentCache.Store(int(uid), 0)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	posts := []Post{}

	err := db.Select(&posts, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_del_flg` = 0 ORDER BY `created_at` DESC LIMIT ?", postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err = makePosts(posts, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	for i := range posts {
		value, ok := userCache.Load(posts[i].UserID)
		if !ok {
			log.Print(err)
			return
		}
		user := value.(User)
		posts[i].User = user
	}

	value, ok := tplCache.Load("getIndex")
	if ok {
		tpl := value.(*template.Template)
		tpl.Execute(w, struct {
			Posts     []Post
			Me        User
			CSRFToken string
			Flash     string
		}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
		return
	}

	tpl := template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	tpl.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
	tplCache.Store("getIndex", tpl)
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := pat.Param(r, "accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?", user.ID, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}
	for i := range posts {
		posts[i].User = user
	}

	value, ok := userCommentCache.Load(user.ID)
	if !ok {
		log.Print(err)
		return
	}
	commentCount := value.(int)

	// commentCount := 0
	// err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	// if err != nil {
	// 	log.Print(err)
	// 	return
	// }

	postIDs := []int{}
	err = db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	for _, postID := range postIDs {
		value, ok := count.Load(postID)
		if !ok {
			log.Print(err)
			return
		}
		commentCount += value.(int)
	}
	// if postCount > 0 {
	// 	s := []string{}
	// 	for range postIDs {
	// 		s = append(s, "?")
	// 	}
	// 	placeholder := strings.Join(s, ", ")

	// 	// convert []int -> []interface{}
	// 	args := make([]interface{}, len(postIDs))
	// 	for i, v := range postIDs {
	// 		args[i] = v
	// 	}

	// 	err = db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
	// 	if err != nil {
	// 		log.Print(err)
	// 		return
	// 	}
	// }

	me := getSessionUser(r)

	value, ok = tplCache.Load("getAccountName")
	if ok {
		tpl := value.(*template.Template)
		tpl.Execute(w, struct {
			Posts          []Post
			User           User
			PostCount      int
			CommentCount   int
			CommentedCount int
			Me             User
		}{posts, user, postCount, commentCount, commentedCount, me})
		return
	}

	tpl := template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	tpl.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
	tplCache.Store("getAccountName", tpl)
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT `id`, `body`, `mime`, `created_at`, `user_id` FROM `posts` WHERE `created_at` <= ? AND `user_del_flg` = 0 ORDER BY `created_at` DESC  LIMIT ?", t.Format(ISO8601Format), postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	for i := range posts {
		value, ok := userCache.Load(posts[i].UserID)
		if !ok {
			log.Print(err)
			return
		}
		user := value.(User)
		posts[i].User = user
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	value, ok := tplCache.Load("getPosts")
	if ok {
		tpl := value.(*template.Template)
		tpl.Execute(w, posts)
		return
	}

	tpl := template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	tpl.Execute(w, posts)
	tplCache.Store("getPosts", tpl)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := pat.Param(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.Select(&results, "SELECT `id`, `body`, `mime`, `created_at`, `user_id` FROM `posts` WHERE `id` = ? AND `user_del_flg` = 0 LIMIT 1", pid)
	if err != nil {
		log.Print(err)
		return
	}

	if len(results) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	for i := range posts {
		value, ok := userCache.Load(posts[i].UserID)
		if !ok {
			log.Print(err)
			return
		}
		user := value.(User)
		posts[i].User = user
	}

	p := posts[0]
	// err = db.Get(&p.User, "SELECT * FROM `users` WHERE `id` = ?", p.UserID)
	// if err != nil {
	// 	log.Print(err)
	// 	return
	// }

	me := getSessionUser(r)

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// ?????????Content-Type?????????????????????????????????????????????
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "??????????????????????????????jpg???png???gif????????????"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`, `user_del_flg`) VALUES (?,?,?,?,?)"
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		"",
		r.FormValue("body"),
		me.DelFlg,
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	count.Store(int(pid), 0)
	postMime.Store(int(pid), mime)

	ext := ""
	if mime == "image/jpeg" {
		ext = "jpg"
	} else if mime == "image/png" {
		ext = "png"
	} else if mime == "image/gif" {
		ext = "gif"
	}
	err = ioutil.WriteFile(fmt.Sprintf("../public/image/%d.%s", pid, ext), filedata, 0644)
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	log.Print("check get image")
	pidStr := pat.Param(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// mime := ""
	// err = db.Get(&mime, "SELECT mime FROM `posts` WHERE `id` = ?", pid)
	// if err != nil {
	// 	log.Print(err)
	// 	return
	// }
	value, ok := postMime.Load(pid)
	if !ok {
		log.Print(err)
		return
	}
	mime, ok := value.(string)
	if !ok {
		log.Print(err)
		return
	}

	ext := pat.Param(r, "ext")

	if ext == "jpg" && mime == "image/jpeg" ||
		ext == "png" && mime == "image/png" ||
		ext == "gif" && mime == "image/gif" {
		w.Header().Set("Content-Type", mime)

		filedata, err := ioutil.ReadFile(fmt.Sprintf("../public/image/%d.%s", pid, ext))
		if err != nil {
			log.Print(err)
			return
		}
		_, err = w.Write(filedata)
		if err != nil {
			log.Print(err)
			return
		}
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_id?????????????????????")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	value, ok := count.Load(postID)
	if !ok {
		log.Print("cannot load comment count")
		return
	}
	commentCount, ok := value.(int)
	if !ok {
		log.Print("failed to type assertion")
		return
	}
	count.Store(postID, commentCount+1)

	value, ok = count.Load(me.ID)
	if !ok {
		log.Print("cannot load user comment count")
		return
	}
	commentCount = value.(int)
	count.Store(me.ID, commentCount+1)

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT `id`, `account_name` FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	if len(r.Form["uid[]"]) > 0 {
		s := []string{}
		for range r.Form["uid[]"] {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		args := make([]interface{}, len(r.Form["uid[]"]))
		for i, v := range r.Form["uid[]"] {
			args[i] = v
		}
		query := "UPDATE `users` SET `del_flg` = 1 WHERE `id` IN (" + placeholder + ")"
		db.Exec(query, args...)

		query = "UPDATE `posts` SET `user_del_flg` = 1 WHERE `user_id` IN (" + placeholder + ")"
		db.Exec(query, args...)
	}

	for _, id := range r.Form["uid[]"] {
		// db.Exec(query, 1, id)
		id, err := strconv.Atoi(id)
		if err != nil {
			log.Print(err)
			return
		}
		value, ok := userCache.Load(id)
		if !ok {
			log.Print(err)
			return
		}
		user := value.(User)
		user.DelFlg = 1
		userCache.Store(id, user)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

type RegexpPattern struct {
	regexp *regexp.Regexp
}

func Regexp(reg *regexp.Regexp) *RegexpPattern {
	return &RegexpPattern{regexp: reg}
}

func (reg *RegexpPattern) Match(r *http.Request) *http.Request {
	ctx := r.Context()
	uPath := pattern.Path(ctx)
	if reg.regexp.MatchString(uPath) {
		values := reg.regexp.FindStringSubmatch(uPath)
		keys := reg.regexp.SubexpNames()

		for i := 1; i < len(keys); i++ {
			ctx = context.WithValue(ctx, pattern.Variable(keys[i]), values[i])
		}

		return r.WithContext(ctx)
	}

	return nil
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&interpolateParams=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	// Post????????????????????????
	posts := []Post{}
	err = db.Select(&posts, "SELECT `posts`.`id` AS `id`, COUNT(`comments`.`id`) AS `count`, `posts`.`mime` AS `mime`, `posts`.`imgdata` AS `imgdata` FROM `posts` LEFT JOIN `comments` ON `posts`.`id` = `comments`.`post_id` GROUP BY `posts`.`id`")
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	for _, p := range posts {
		ext := ""
		if p.Mime == "image/jpeg" {
			ext = "jpg"
		} else if p.Mime == "image/png" {
			ext = "png"
		} else if p.Mime == "image/gif" {
			ext = "gif"
		}
		filename := fmt.Sprintf("../public/image/%d.%s", p.ID, ext)
		if _, err := os.Stat(filename); os.IsNotExist(err) {
			err = ioutil.WriteFile(filename, p.Imgdata, 0644)
			if err != nil {
				log.Fatalf("Failed to write image data to file: %s.", err.Error())
			}
		}
		count.Store(p.ID, p.CommentCount)
		postMime.Store(p.ID, p.Mime)
	}

	// User????????????????????????
	users := []User{}
	err = db.Select(&users, "SELECT * FROM `users`")
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	for _, user := range users {
		userCache.Store(user.ID, user)
	}

	// Comment????????????????????????
	commentCounts := []struct {
		UserID       int `db:"user_id"`
		CommentCount int `db:"count"`
	}{}
	err = db.Select(&commentCounts, "SELECT `user_id`, COUNT(id) AS count FROM `comments` GROUP BY `user_id`")
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	for _, commentCount := range commentCounts {
		userCommentCache.Store(commentCount.UserID, commentCount.CommentCount)
	}

	go func() {
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	mux := goji.NewMux()

	mux.HandleFunc(pat.Get("/initialize"), getInitialize)
	mux.HandleFunc(pat.Get("/login"), getLogin)
	mux.HandleFunc(pat.Post("/login"), postLogin)
	mux.HandleFunc(pat.Get("/register"), getRegister)
	mux.HandleFunc(pat.Post("/register"), postRegister)
	mux.HandleFunc(pat.Get("/logout"), getLogout)
	mux.HandleFunc(pat.Get("/"), getIndex)
	mux.HandleFunc(pat.Get("/posts"), getPosts)
	mux.HandleFunc(pat.Get("/posts/:id"), getPostsID)
	mux.HandleFunc(pat.Post("/"), postIndex)
	mux.HandleFunc(pat.Get("/image/:id.:ext"), getImage)
	mux.HandleFunc(pat.Post("/comment"), postComment)
	mux.HandleFunc(pat.Get("/admin/banned"), getAdminBanned)
	mux.HandleFunc(pat.Post("/admin/banned"), postAdminBanned)
	mux.HandleFunc(Regexp(regexp.MustCompile(`^/@(?P<accountName>[a-zA-Z]+)$`)), getAccountName)
	mux.Handle(pat.Get("/*"), http.FileServer(http.Dir("../public")))

	log.Print("ready for running server")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
