package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/russross/blackfriday"
)

type PageType int

const (
	Post PageType = iota
	Page
	Snippet
)

type Article struct {
	Author       string
	DateModified *time.Time
	DateUpdated  *time.Time
	Title        string
	Content      string
	Description  string
	Filename     string
	Link         string
	Identifier   string
	Snippet      bool
	Type         PageType
	Draft        bool
	Tags         []string
	AppID        string
}

func (a Article) HasTag(aTag string) bool {
	for _, tag := range a.Tags {
		if aTag == tag {
			return true
		}
	}

	return false
}

func (a Article) BasePath() string {
	switch a.Type {
	case Post, Snippet:
		return path.Join(strconv.Itoa(a.DateModified.Year()), fmt.Sprintf("%02d", int(a.DateModified.Month())))
	case Page:
		return ""
	}

	return ""
}

func (a Article) FullPath() string {
	return path.Join(a.BasePath(), a.Filename)
}

type Articles []*Article

func (a Articles) Len() int {
	return len(a)
}

func (a Articles) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a Articles) Less(i, j int) bool {
	left := *(a[i].DateModified)
	right := *(a[j].DateModified)

	return right.Before(left)
}

func ReadArticle(reader *bufio.Reader) Article {
	article := Article{}

	line, lineErr := reader.ReadString('\n')

	article.Type = Post

	for lineErr == nil {

		if strings.HasPrefix(line, "---") {
			break
		}

		values := strings.SplitN(line, ":", 2)

		if len(values) < 2 {
			line, lineErr = reader.ReadString('\n')
			continue
		}

		key, value := values[0], values[1]

		key = strings.Trim(key, " \t\r\n")
		value = strings.Trim(value, " \t\r\n")

		switch key {
		case "title":
			article.Title = value
		case "author":
			article.Author = value
		case "description":
			article.Description = value
		case "link":
			article.Link = value
		case "date":
			dateStr := value
			modTime, timeErr := time.Parse(time.RFC3339, dateStr)
			if timeErr == nil {
				article.DateModified = &modTime
			} else {
				log.Fatalf("Could not parse date \"%s\"", dateStr)
			}

		case "updated":
			dateStr := value
			modTime, timeErr := time.Parse(time.RFC3339, dateStr)
			if timeErr == nil {
				article.DateUpdated = &modTime
			} else {
				log.Fatalf("Could not parse date \"%s\"", dateStr)
			}

		case "appid":
			article.AppID = value
		case "type":
			switch value {
			case "Post":
				article.Type = Post
			case "Page":
				article.Type = Page
			case "Snippet":
				article.Type = Snippet
			}

		case "tags":
			fieldsFunc := func(divider rune) bool {
				return unicode.IsSpace(divider) || divider == ',' || divider == ';'
			}

			for _, tag := range strings.FieldsFunc(value, fieldsFunc) {
				article.Tags = append(article.Tags, strings.ToLower(tag))
			}
		}

		line, lineErr = reader.ReadString('\n')
	}

	contentBuffer := bytes.NewBufferString("")
	line, lineErr = reader.ReadString('\n')

	for lineErr == nil {
		contentBuffer.WriteString(line)
		contentBuffer.WriteString("\n")

		line, lineErr = reader.ReadString('\n')
	}

	htmlFlags := 0
	// htmlFlags |= blackfriday.HTML_USE_XHTML
	htmlFlags |= blackfriday.HTML_USE_SMARTYPANTS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_FRACTIONS
	htmlFlags |= blackfriday.HTML_SMARTYPANTS_LATEX_DASHES
	// htmlFlags |= blackfriday.HTML_SANITIZE_OUTPUT
	renderer := blackfriday.HtmlRenderer(htmlFlags, "", "")
	extensions := 0
	extensions |= blackfriday.EXTENSION_NO_INTRA_EMPHASIS
	extensions |= blackfriday.EXTENSION_TABLES
	extensions |= blackfriday.EXTENSION_FENCED_CODE
	extensions |= blackfriday.EXTENSION_AUTOLINK
	extensions |= blackfriday.EXTENSION_STRIKETHROUGH
	extensions |= blackfriday.EXTENSION_SPACE_HEADERS
	extensions |= blackfriday.EXTENSION_HEADER_IDS

	md := blackfriday.Markdown(contentBuffer.Bytes(), renderer, extensions)

	article.Content = string(md)

	if len(article.Description) == 0 {
		article.Description = article.Content
	}

	return article
}
