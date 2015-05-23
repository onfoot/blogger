package main

import (
	"bufio"
	"bytes"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"text/template"
	"time"
)

var blogTitle = flag.String("title", "blog", "Blog title")
var destinationExt = flag.String("extension", "", "Destination file extension")
var sourcePath = flag.String("source", "source", "Source directory")
var destinationPath = flag.String("destination", "destination", "Destination directory")
var siteRoot = flag.String("root", "/", "Site root path")
var templatePrint = flag.String("print", "", "Print out a template for a snippet or a blog post")
var templateAuthor = flag.String("author", "", "Set a default post author")

const templateFileName = "templates/template.html"
const rssTemplateFileName = "templates/rsstemplate.html"

func main() {

	flag.Parse()

	if *templatePrint != "" {
		var article Article
		now := time.Now().Add(15 * time.Minute)
		article.DateModified = &now

		if *templateAuthor != "" {
			article.Author = *templateAuthor
		}

		switch *templatePrint {
		case "post":
			article.Title = "Blog post"
			article.Type = Post
			break
		case "snippet":
			article.Type = Snippet
			break
		default:
			log.Fatal("post and snippet are the only allowed parameters for -print")
		}

		article.Print()

		return
	}

	log.Printf("Blog title is %s", *blogTitle)

	destinationDir, destinationDirErr := os.Open(*destinationPath)

	if destinationDirErr != nil {
		log.Fatal("Destination directory could not be open: ", destinationDirErr)
	}

	defer destinationDir.Close()

	funcMap := template.FuncMap{
		"longDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("Monday, _2 January 2006, 15:04") },
		"snippetDate":  func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2 2006, 15:04") },
		"shortDate":    func(args ...interface{}) string { return args[0].(*time.Time).Format("Jan _2, 2006") },
		"atomDate":     func(args ...interface{}) string { return args[0].(*time.Time).Format("2006-01-02T15:04:05Z07:00") },
		"Snippet":      func(args ...interface{}) bool { return args[0].(*Article).Type == Snippet },
		"last":         func(index, count int) bool { return index == count-1 },
		"tagIndexName": func(tag string) string { return "tag-" + tag + *destinationExt },
		"path": func(article Article) string {
			return article.FullPath()
		},
	}

	mainTemplate := template.Must(template.New("template.html").Funcs(funcMap).ParseFiles(path.Join(*sourcePath, templateFileName)))
	mainRssTemplate := template.Must(template.New("rsstemplate.html").Funcs(funcMap).ParseFiles(path.Join(*sourcePath, rssTemplateFileName)))

	now := time.Now()

	postDir := path.Join(*sourcePath, "posts")

	sourceFiles, sourceFilesErr := ioutil.ReadDir(postDir)
	if sourceFilesErr != nil {
		log.Fatal("Source directory could not be read")
	}

	var articles Articles

	var indexArticles Articles
	var feedArticles Articles
	var snippetArticles Articles

	for _, sourceFileInfo := range sourceFiles {

		ext := path.Ext(sourceFileInfo.Name())

		if sourceFileInfo.IsDir() || (ext != ".markdown" && ext != ".md") {
			continue
		}

		name := strings.TrimSuffix(path.Base(sourceFileInfo.Name()), ext)

		sourceFile, _ := ioutil.ReadFile(path.Join(postDir, sourceFileInfo.Name()))

		sourceBuffer := bytes.NewBuffer(sourceFile)

		mdReader := bufio.NewReader(sourceBuffer)

		line, lineErr := mdReader.ReadString('\n')

		if lineErr != nil {
			continue
		}

		var article Article

		if strings.HasPrefix(line, "---") {
			article = ReadArticle(mdReader)
			article.Filename = name + *destinationExt
		} else {
			log.Fatal("Bad article")
			continue
		}

		if article.DateModified == nil {
			article.DateModified = new(time.Time)
			*article.DateModified = sourceFileInfo.ModTime()
		}

		article.Identifier = name

		if strings.HasSuffix(name, "draft") {
			article.Draft = true
		}

		articles = append(articles, &article)

		if article.Type == Page {
			continue
		}

		if article.Draft {
			continue
		}

		if article.Type == Post {
			feedArticles = append(feedArticles, &article)
		}

		if article.Type == Snippet {
			snippetArticles = append(snippetArticles, &article)
		}

		indexArticles = append(indexArticles, &article)
	}

	tags := map[string]bool{}

	sort.Sort(articles)
	sort.Sort(indexArticles)
	sort.Sort(feedArticles)
	sort.Sort(snippetArticles)

	indexBuffer := bytes.NewBufferString("")
	rssIndexBuffer := bytes.NewBufferString("")
	snippetrssIndexBuffer := bytes.NewBufferString("")

	mainTemplate.Execute(indexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": siteRoot, "Articles": indexArticles, "CreatedTime": now})
	mainRssTemplate.Execute(rssIndexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": siteRoot, "File": "index.xml", "Articles": feedArticles, "CreatedTime": &now})
	mainRssTemplate.Execute(snippetrssIndexBuffer, map[string]interface{}{"Title": blogTitle, "Home": true, "Root": siteRoot, "File": "snippets.xml", "Articles": snippetArticles, "CreatedTime": &now})

	for _, article := range articles {

		destFileBuffer := bytes.NewBufferString("")

		mainTemplate.Execute(destFileBuffer, map[string]interface{}{"BlogTitle": blogTitle, "Article": article, "Title": string(article.Title + " – " + *blogTitle), "Home": false, "Root": siteRoot})

		for _, tag := range article.Tags {
			tags[tag] = true
		}

		destinationFileName := path.Join(destinationDir.Name(), article.FullPath())

		os.MkdirAll(path.Join(destinationDir.Name(), article.BasePath()), os.ModePerm)

		writeErr := ioutil.WriteFile(destinationFileName, destFileBuffer.Bytes(), os.ModePerm)

		if writeErr != nil {
			log.Println(writeErr)
		}
	}

	indexFileName := path.Join(destinationDir.Name(), "index.html")
	rssIndexFileName := path.Join(destinationDir.Name(), "index.xml")
	snippetIndexFileName := path.Join(destinationDir.Name(), "snippets.xml")

	ioutil.WriteFile(indexFileName, indexBuffer.Bytes(), os.ModePerm)
	ioutil.WriteFile(rssIndexFileName, rssIndexBuffer.Bytes(), os.ModePerm)
	ioutil.WriteFile(snippetIndexFileName, snippetrssIndexBuffer.Bytes(), os.ModePerm)

	for tag := range tags {

		tagIndexBuffer := bytes.NewBufferString("")
		var tagArticles Articles

		for _, article := range indexArticles {

			if !article.HasTag(tag) {
				continue
			}

			tagArticles = append(tagArticles, article)
		}

		mainTemplate.Execute(tagIndexBuffer, map[string]interface{}{"Articles": tagArticles, "Title": "Tag: " + tag + " – " + *blogTitle, "Home": false, "Root": siteRoot})

		tagIndexFileName := path.Join(destinationDir.Name(), "tag-"+tag+*destinationExt)
		ioutil.WriteFile(tagIndexFileName, tagIndexBuffer.Bytes(), os.ModePerm)
	}
}
