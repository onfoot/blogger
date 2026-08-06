package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"net/http"
	"sync"

	"macbirdie.net/blogger/auth"
	"macbirdie.net/blogger/micropub"
	"macbirdie.net/blogger/post"

	blackfriday "github.com/russross/blackfriday/v2"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/term"
)

var blogTitle = flag.String("title", "blog", "Blog title")
var destinationExt = flag.String("extension", "", "Destination file extension")
var postsPath = flag.String("posts", "posts", "Posts directory, comma separated for multiple directories")
var templatesPath = flag.String("templates", "templates", "Templates directory")
var destinationPath = flag.String("destination", "destination", "Destination directory")
var siteRoot = flag.String("root", "/", "Site root path")
var templatePrint = flag.String("print", "", "Print out a template for a snippet, blog post or a page")
var templateAuthor = flag.String("author", "", "Set a default post author")
var listen = flag.Bool("listen", false, "Listen to changes in post directories and regenerate")
var tagfeeds = flag.String("tagfeeds", "", "Generate RSS feeds for specified tags (comma-separated)")
var micropubURL = flag.String("micropub-url", "", "Micropub endpoint base URL (e.g. https://example.com/micropub); adds <link> tags to templates")
var dbPath = flag.String("db", ".", "Directory for the auth database (blogger.db is created here)")
var addUser = flag.String("adduser", "", "Add a new user to the auth database (prompts for password)")
var updateUser = flag.String("updateuser", "", "Update an existing user's password (prompts for password)")
var listUsers = flag.Bool("listusers", false, "List all users in the auth database")
var serveAddr = flag.String("serve", "", "Start Micropub HTTP server on this address (e.g. :8080)")

// generateMu prevents concurrent generate() calls from the fsnotify watcher
// and the Micropub OnChange callback running simultaneously.
var generateMu sync.Mutex

func safeGenerate() {
	generateMu.Lock()
	defer generateMu.Unlock()
	generate()
}

const templateFileName = "template.html"
const rssTemplateFileName = "rsstemplate.html"

var postExtensions = []string{".md", ".markdown", ".txt"}

func containsString(haystack []string, needle string) bool {
	for _, hay := range haystack {
		if hay == needle {
			return true
		}
	}

	return false
}

func expandHomePath(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	u, err := user.Current()
	if err != nil {
		log.Fatalf("Could not determine home directory: %v", err)
	}
	return filepath.Join(u.HomeDir, p[2:])
}

func generate() {

	log.Printf("Generating blog: %s", *blogTitle)

	destinationDir, err := os.Open(*destinationPath)
	if err != nil {
		log.Fatal("Destination directory could not be opened: ", err)
	}
	defer destinationDir.Close()

	funcMap := template.FuncMap{
		"longDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("Monday, _2 January 2006, 15:04") },
		"snippetDate":  func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2 2006, 15:04") },
		"shortDate":    func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2, 2006") },
		"atomDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("2006-01-02T15:04:05Z07:00") },
		"Snippet":      func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Snippet },
		"Post":         func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Post },
		"Page":         func(args ...interface{}) bool { return args[0].(*post.Article).Type == post.Page },
		"last":         func(index, count int) bool { return index == count-1 },
		"tagIndexName": func(tag string) string { return "tag-" + tag + *destinationExt },
		"path":         func(article post.Article) string { return article.FullPath() },
	}

	mainTemplate := template.Must(template.New("template.html").Funcs(funcMap).ParseFiles(path.Join(*templatesPath, templateFileName)))
	mainRssTemplate := template.Must(template.New("rsstemplate.html").Funcs(funcMap).ParseFiles(path.Join(*templatesPath, rssTemplateFileName)))

	now := time.Now()

	type PostFile struct {
		Name      string
		Extension string
		Path      string
	}

	var sourceFiles []PostFile

	for _, postDir := range strings.Split(*postsPath, ",") {
		postDir = expandHomePath(strings.TrimSpace(postDir))

		walkFunc := func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				log.Fatalf("Post directory %q not found", filePath)
			}

			if info.IsDir() {
				return nil
			}

			filename := path.Base(info.Name())
			ext := path.Ext(filename)

			if !containsString(postExtensions, ext) {
				return nil
			}

			for {
				filename = strings.TrimSuffix(filename, ext)
				ext = path.Ext(filename)

				if !containsString(postExtensions, ext) {
					break
				}
			}

			sourceFiles = append(sourceFiles, PostFile{Name: filename, Extension: ext, Path: filePath})
			return nil
		}

		if err := filepath.Walk(postDir, walkFunc); err != nil {
			log.Printf("Error walking directory %q: %v", postDir, err)
		}
	}

	var articles, indexArticles, feedArticles, snippetArticles post.Articles

	htmlPrefix := strings.TrimSuffix(*siteRoot, "/")
	log.Println("Using prefix", htmlPrefix)

	renderer := blackfriday.NewHTMLRenderer(blackfriday.HTMLRendererParameters{
		Flags:          blackfriday.Smartypants | blackfriday.SmartypantsFractions | blackfriday.SmartypantsLatexDashes,
		AbsolutePrefix: htmlPrefix,
	})

	extensions := blackfriday.NoIntraEmphasis |
		blackfriday.Tables |
		blackfriday.FencedCode |
		blackfriday.Autolink |
		blackfriday.Strikethrough |
		blackfriday.SpaceHeadings |
		blackfriday.HeadingIDs |
		blackfriday.Footnotes

	for _, sourceFile := range sourceFiles {

		file, fileError := os.Open(sourceFile.Path)
		if fileError != nil {
			log.Printf("Skipping %v due to error: %v", sourceFile.Path, fileError)
			continue
		}

		article, readErr := post.ReadArticle(bufio.NewReader(file))
		file.Close()

		if readErr != nil {
			log.Printf("Skipping file %v due to parse error: %v", sourceFile.Path, readErr)
			continue
		}

		article.Content = string(blackfriday.Run(
			article.RawContent,
			blackfriday.WithRenderer(renderer),
			blackfriday.WithExtensions(extensions),
		))

		article.Filename = sourceFile.Name + *destinationExt

		if article.DateModified == nil {
			article.DateModified = new(time.Time)
		}

		article.Identifier = sourceFile.Name

		articles = append(articles, &article)

		if article.Type == post.Page || article.Draft {
			continue
		}

		if article.Type == post.Post {
			feedArticles = append(feedArticles, &article)
		}

		if article.Type == post.Snippet {
			snippetArticles = append(snippetArticles, &article)
		}

		indexArticles = append(indexArticles, &article)
	}

	tags := map[post.Tag]bool{}

	sort.Sort(articles)
	sort.Sort(indexArticles)
	sort.Sort(feedArticles)
	sort.Sort(snippetArticles)

	indexBuffer := new(bytes.Buffer)
	rssIndexBuffer := new(bytes.Buffer)
	snippetrssIndexBuffer := new(bytes.Buffer)

	micropubEndpoint := *micropubURL
	tokenEndpoint := ""
	authEndpoint := ""
	if micropubEndpoint != "" {
		base := strings.TrimRight(micropubEndpoint, "/")
		tokenEndpoint = base + "/token"
		authEndpoint = base + "/auth"
	}

	if err := mainTemplate.Execute(indexBuffer, map[string]interface{}{
		"Title":            blogTitle,
		"Home":             true,
		"Root":             *siteRoot,
		"Articles":         indexArticles,
		"CreatedTime":      now,
		"MicropubURL":      micropubEndpoint,
		"TokenEndpointURL": tokenEndpoint,
		"AuthEndpointURL":  authEndpoint,
	}); err != nil {
		log.Printf("Error rendering index: %v", err)
	}

	if err := mainRssTemplate.Execute(rssIndexBuffer, map[string]interface{}{
		"Title":       blogTitle,
		"Home":        true,
		"Root":        *siteRoot,
		"File":        "index.xml",
		"Articles":    feedArticles,
		"CreatedTime": &now,
	}); err != nil {
		log.Printf("Error rendering RSS index: %v", err)
	}

	if err := mainRssTemplate.Execute(snippetrssIndexBuffer, map[string]interface{}{
		"Title":       blogTitle,
		"Home":        true,
		"Root":        *siteRoot,
		"File":        "snippets.xml",
		"Articles":    snippetArticles,
		"CreatedTime": &now,
	}); err != nil {
		log.Printf("Error rendering snippet RSS: %v", err)
	}

	for _, article := range articles {

		destFileBuffer := new(bytes.Buffer)

		if err := mainTemplate.Execute(destFileBuffer, map[string]interface{}{
			"BlogTitle":        blogTitle,
			"Article":          article,
			"Title":            article.Title + " – " + *blogTitle,
			"Home":             false,
			"Root":             *siteRoot,
			"MicropubURL":      micropubEndpoint,
			"TokenEndpointURL": tokenEndpoint,
			"AuthEndpointURL":  authEndpoint,
		}); err != nil {
			log.Printf("Error rendering article %v: %v", article.Filename, err)
		}

		for _, tag := range article.Tags {
			tags[tag] = true
		}

		destinationFileName := path.Join(destinationDir.Name(), article.FullPath())

		if err := os.MkdirAll(path.Join(destinationDir.Name(), article.BasePath()), 0755); err != nil {
			log.Printf("Could not create directory for %v: %v", destinationFileName, err)
			continue
		}

		if err := os.WriteFile(destinationFileName, destFileBuffer.Bytes(), 0644); err != nil {
			log.Printf("Could not write file %v: %v", destinationFileName, err)
		}
	}

	for name, buf := range map[string]*bytes.Buffer{
		path.Join(destinationDir.Name(), "index.html"):   indexBuffer,
		path.Join(destinationDir.Name(), "index.xml"):    rssIndexBuffer,
		path.Join(destinationDir.Name(), "snippets.xml"): snippetrssIndexBuffer,
	} {
		if err := os.WriteFile(name, buf.Bytes(), 0644); err != nil {
			log.Printf("Could not write %v: %v", name, err)
		}
	}

	tagFeedsEnabled := map[string]bool{}
	for _, tagEnabled := range strings.Split(*tagfeeds, ",") {
		tagFeedsEnabled[tagEnabled] = true
	}

	for tag := range tags {

		tagIndexBuffer := new(bytes.Buffer)
		var tagArticles post.Articles

		for _, article := range indexArticles {
			if article.HasTag(tag.Name) {
				tagArticles = append(tagArticles, article)
			}
		}

		if err := mainTemplate.Execute(tagIndexBuffer, map[string]interface{}{
			"Articles":         tagArticles,
			"Title":            "Tag: " + tag.Name + " – " + *blogTitle,
			"Home":             false,
			"Root":             *siteRoot,
			"MicropubURL":      micropubEndpoint,
			"TokenEndpointURL": tokenEndpoint,
			"AuthEndpointURL":  authEndpoint,
		}); err != nil {
			log.Printf("Error rendering tag %v index: %v", tag.Name, err)
		}

		if tagFeedsEnabled[tag.OriginalName] {
			tagFeedBuffer := new(bytes.Buffer)
			feedFileName := "index-tag-" + tag.FileName() + ".xml"

			if err := mainRssTemplate.Execute(tagFeedBuffer, map[string]interface{}{
				"Title":       blogTitle,
				"Home":        true,
				"Root":        *siteRoot,
				"File":        feedFileName,
				"Articles":    tagArticles,
				"CreatedTime": &now,
			}); err != nil {
				log.Printf("Error rendering tag %v feed: %v", tag.Name, err)
			}

			tagFeedFileName := path.Join(destinationDir.Name(), feedFileName)
			if err := os.WriteFile(tagFeedFileName, tagFeedBuffer.Bytes(), 0644); err != nil {
				log.Printf("Could not write %v: %v", tagFeedFileName, err)
			}
		}

		tagIndexFileName := path.Join(destinationDir.Name(), "tag-"+tag.FileName()+*destinationExt)
		if err := os.WriteFile(tagIndexFileName, tagIndexBuffer.Bytes(), 0644); err != nil {
			log.Printf("Could not write %v: %v", tagIndexFileName, err)
		}
	}
}

func watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal("Couldn't watch the post directories: ", err)
	}
	defer watcher.Close()

	watcherDone := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					log.Println("Modified file:", event.Name)
					safeGenerate()
				}
			case err := <-watcher.Errors:
				log.Println("Got error:", err)
			}
		}
	}()

	var watchedDirs []string

	for _, postDir := range strings.Split(*postsPath, ",") {
		postDir = expandHomePath(strings.TrimSpace(postDir))

		walkFunc := func(filePath string, info os.FileInfo, err error) error {
			if err != nil || !info.IsDir() {
				return nil
			}
			watchedDirs = append(watchedDirs, filePath)
			return nil
		}

		filepath.Walk(postDir, walkFunc)
	}

	watchedDirs = append(watchedDirs, *templatesPath)

	for _, watchedDir := range watchedDirs {
		watcher.Add(watchedDir)
	}

	log.Printf("Listening to changes in directories: %s…", strings.Join(watchedDirs, ", "))

	<-watcherDone
}

func promptPassword(prompt string) (string, error) {
	fmt.Print(prompt)
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(raw), nil
}

func handleUserManagement() bool {
	if *addUser == "" && *updateUser == "" && !*listUsers {
		return false
	}

	db, err := auth.OpenDB(filepath.Join(*dbPath, "blogger.db"))
	if err != nil {
		log.Fatalf("Could not open auth database: %v", err)
	}
	defer db.Close()

	switch {
	case *addUser != "":
		password, err := promptPassword(fmt.Sprintf("Password for %q: ", *addUser))
		if err != nil {
			log.Fatalf("Could not read password: %v", err)
		}
		if err := auth.AddUser(db, *addUser, password); err != nil {
			log.Fatalf("Could not add user: %v", err)
		}
		fmt.Printf("User %q added.\n", *addUser)

	case *updateUser != "":
		password, err := promptPassword(fmt.Sprintf("New password for %q: ", *updateUser))
		if err != nil {
			log.Fatalf("Could not read password: %v", err)
		}
		if err := auth.UpdateUser(db, *updateUser, password); err != nil {
			log.Fatalf("Could not update user: %v", err)
		}
		fmt.Printf("User %q updated.\n", *updateUser)

	case *listUsers:
		users, err := auth.ListUsers(db)
		if err != nil {
			log.Fatalf("Could not list users: %v", err)
		}
		if len(users) == 0 {
			fmt.Println("No users found.")
		}
		for _, u := range users {
			fmt.Println(u)
		}
	}

	return true
}

func main() {
	flag.Parse()

	if handleUserManagement() {
		return
	}

	if *templatePrint != "" {
		var article post.Article
		now := time.Now().Add(15 * time.Minute)
		article.DateModified = &now

		article.Draft = true

		if *templateAuthor != "" {
			article.Author = *templateAuthor
		}

		switch *templatePrint {
		case "page":
			article.Title = "Hello world"
			article.Type = post.Page
		case "post":
			article.Title = "Blog post"
			article.Type = post.Post
		case "snippet":
			article.Type = post.Snippet
		default:
			log.Fatal("post, snippet and page are the only allowed parameters for -print")
		}

		article.Print()

		return
	}

	generate()

	if *serveAddr != "" {
		db, err := auth.OpenDB(filepath.Join(*dbPath, "blogger.db"))
		if err != nil {
			log.Fatalf("Could not open auth database for server: %v", err)
		}
		defer db.Close()

		// Use the first posts directory as the write target for new posts.
		firstPostsDir := expandHomePath(strings.TrimSpace(strings.SplitN(*postsPath, ",", 2)[0]))

		srv := &micropub.Server{
			DB:       db,
			PostsDir: firstPostsDir,
			SiteRoot: *siteRoot,
			DestExt:  *destinationExt,
			OnChange: safeGenerate,
		}

		if *listen {
			// Run the file watcher in a goroutine so the HTTP server can block.
			go watch()
		}

		log.Printf("Micropub server listening on %s", *serveAddr)
		if err := http.ListenAndServe(*serveAddr, srv.Handler()); err != nil {
			log.Fatalf("Micropub server: %v", err)
		}
		return
	}

	if *listen {
		watch()
	}
}
