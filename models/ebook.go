package models

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/TruthHun/BookStack/models/store"
	"github.com/TruthHun/BookStack/utils"
	"github.com/TruthHun/converter/converter"
	"github.com/TruthHun/gotil/cryptil"
	"github.com/TruthHun/gotil/util"
	"github.com/astaxie/beego"
	"github.com/astaxie/beego/httplib"
	"github.com/astaxie/beego/orm"
)

type Ebook struct {
	Id            int
	Title         string    // 电子书名称
	Keywords      string    // 关键字
	Description   string    // 摘要
	Path          string    // 文件路径。如果是网站生成的电子书，则为电子书的路径，否则为URL地址
	BookID        int       `orm:"default(0);column(book_id);index"` // 所属书籍ID
	Ext           string    `orm:"size(8);index"`                    // 文件扩展名
	Status        int       `orm:"default(0);index"`                 // 0：待处理； 1: 转换中；2: 转换完成
	Size          int64     `orm:"default(0)"`                       // 电子书大小
	DownloadCount int       `orm:"default(0)"`                       // 电子书被下载次数
	CreatedAt     time.Time `orm:"auto_now_add;type(datetime)"`
	UpdatedAt     time.Time `orm:"auto_now;type(datetime)"`
}

var convert2ebookRunning = false

const (
	EBookStatusPending     = 0 // 待处理
	EBookStatusProccessing = 1 // 处理中
	EBookStatusSuccess     = 2 // 转换成功
	EBookStatusFailure     = 3 // 失败
)

func NewEbook() *Ebook {
	return &Ebook{}
}

func (m *Ebook) GetEBookByBookID(bookID int) (books []Ebook) {
	if bookID <= 0 {
		return
	}

	if _, err := orm.NewOrm().QueryTable(m).Filter("book_id", bookID).All(&books); err != nil && err != orm.ErrNoRows {
		beego.Error(err)
	}
	return
}

func (m *Ebook) Get2Download(bookId int, ext string) (ebook Ebook) {
	o := orm.NewOrm()
	o.QueryTable(m).Filter("book_id", bookId).Filter("ext", ext).OrderBy("-id").One(&ebook)
	if ebook.Id > 0 {
		ebook.DownloadCount = ebook.DownloadCount + 1
		o.Update(&ebook)
	}
	return
}

func (m *Ebook) GetEBook(id int) (book Ebook) {
	if id <= 0 {
		return
	}
	err := orm.NewOrm().QueryTable(m).Filter("id", id).One(&book)
	if err != nil {
		beego.Error(err)
	}
	return
}

// 添加书籍到电子书生成队列
func (m *Ebook) AddToGenerate(bookID int) (err error) {
	var (
		ebooks []Ebook
		exts   = []string{".pdf", ".mobi", ".epub"}
	)

	b, _ := NewBook().Find(bookID)
	if b == nil || b.BookId == 0 {
		return errors.New("书籍不存在")
	}
	for _, ext := range exts {
		ebooks = append(ebooks, Ebook{
			Title:       b.BookName,
			Keywords:    b.Label,
			Description: b.Description,
			BookID:      bookID,
			Ext:         ext,
			Status:      EBookStatusPending,
		})
	}

	if _, err = orm.NewOrm().InsertMulti(len(ebooks), &ebooks); err != nil {
		beego.Error(err)
	}
	return
}

// 电子书状态（最新的状态）
func (m *Ebook) Stats(bookID int) (stats map[string]Ebook) {
	var (
		ebooks []Ebook
		limit  = 4 // 先默认为4，即四个扩展名：.pdf,.epub,.mobi,.docx
	)
	stats = make(map[string]Ebook)
	o := orm.NewOrm()
	o.QueryTable(m).Filter("book_id", bookID).OrderBy("-id").Limit(limit).All(&ebooks)
	if len(ebooks) == 0 {
		return
	}

	for _, ebook := range ebooks {
		if _, ok := stats[ebook.Ext]; !ok {
			stats[ebook.Ext] = ebook
		}
	}
	return
}

// 查询书籍是否处于完成状态。失败也是完成状态的一种。
func (m *Ebook) IsFinish(bookID int) (ok bool) {
	count, err := orm.NewOrm().QueryTable(m).Filter("book_id", bookID).Filter("status__in", EBookStatusPending, EBookStatusProccessing).Count()
	if err != nil {
		beego.Error(err)
		return
	}
	return count == 0
}

func (m *Ebook) CheckAndGenerateEbook() {
	if convert2ebookRunning {
		return
	}
	convert2ebookRunning = true
	o := orm.NewOrm()
	o.QueryTable(m).Filter("book_id__gt", 0).Filter("status", EBookStatusProccessing).Update(orm.Params{"status": EBookStatusPending})
	for {
		var ebook Ebook
		o.QueryTable(m).Filter("book_id__gt", 0).Filter("status", EBookStatusPending).OrderBy("id").One(&ebook)
		if ebook.Id > 0 {
			// 根据电子书的ID，查找现有的电子书的队列
			ebook.Status = EBookStatusProccessing
			o.Update(&ebook)
			m.generate(ebook.BookID)
		}
		time.Sleep(5 * time.Second)
	}
}

//离线文档生成
func (m *Ebook) generate(bookID int) {
	book, err := NewBook().Find(bookID)
	if err != nil {
		beego.Error(err)
		return
	}

	exts := []string{".pdf", ".epub", ".mobi"}
	debug := true
	if beego.AppConfig.String("runmode") == "prod" {
		debug = false
	}

	nickname := NewMember().GetNicknameByUid(book.MemberId)
	docs, err := NewDocument().FindListByBookId(book.BookId)
	if err != nil {
		beego.Error(err)
		return
	}
	cfg := converter.Config{
		Contributor: beego.AppConfig.String("exportCreator"),
		Cover:       "",
		Creator:     beego.AppConfig.String("exportCreator"),
		Timestamp:   book.ReleaseTime.Format("2006-01-02"),
		Description: book.Description,
		Header:      beego.AppConfig.String("exportHeader"),
		Footer:      beego.AppConfig.String("exportFooter"),
		Identifier:  book.Identify,
		Language:    "zh-CN",
		Publisher:   beego.AppConfig.String("exportCreator"),
		Title:       book.BookName,
		Format:      exts,
		FontSize:    beego.AppConfig.String("exportFontSize"),
		PaperSize:   beego.AppConfig.String("exportPaperSize"),
		More: []string{
			"--pdf-page-margin-bottom", beego.AppConfig.DefaultString("exportMarginBottom", "72"),
			"--pdf-page-margin-left", beego.AppConfig.DefaultString("exportMarginLeft", "72"),
			"--pdf-page-margin-right", beego.AppConfig.DefaultString("exportMarginRight", "72"),
			"--pdf-page-margin-top", beego.AppConfig.DefaultString("exportMarginTop", "72"),
		},
	}

	folder := fmt.Sprintf("cache/books/%v/", book.Identify)
	os.MkdirAll(folder, os.ModePerm)
	if !debug {
		defer os.RemoveAll(folder)
	}

	if beego.AppConfig.DefaultBool("exportCustomCover", false) {
		// 生成书籍封面
		if err = utils.RenderCoverByBookIdentify(book.Identify); err != nil {
			beego.Error(err)
		}
		cover := "cover.png"
		if _, err = os.Stat(folder + cover); err != nil {
			cover = ""
		}
		// 用相对路径
		cfg.Cover = cover
	}

	//生成致谢内容
	beego.Info("加载致谢模板内容(可删除和更改)：views/ebook/statement.html")
	statementFile := "ebook/statement.html"
	_, err = os.Stat("views/" + statementFile)
	if err != nil {
		beego.Info("致谢模板不存在，跳过...")
		beego.Error(err)
	} else {
		if htmlStr, err := utils.ExecuteViewPathTemplate(statementFile, map[string]interface{}{"Model": book, "Nickname": nickname, "Date": cfg.Timestamp}); err == nil {
			h1Title := "说明"
			if doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr)); err == nil {
				h1Title = doc.Find("h1").Text()
			}
			toc := converter.Toc{
				Id:    time.Now().Nanosecond(),
				Pid:   0,
				Title: h1Title,
				Link:  "statement.html",
			}
			htmlname := folder + toc.Link
			ioutil.WriteFile(htmlname, []byte(htmlStr), os.ModePerm)
			cfg.Toc = append(cfg.Toc, toc)
		}
	}

	docStore := NewDocumentStore()
	baseUrl := "http://localhost:" + strconv.Itoa(beego.AppConfig.DefaultInt("httport", 8181))
	for _, doc := range docs {
		content := strings.TrimSpace(docStore.GetFiledById(doc.DocumentId, "content"))
		if utils.GetTextFromHtml(content) == "" { //内容为空，渲染文档内容，并再重新获取文档内容
			utils.RenderDocumentById(doc.DocumentId)
			orm.NewOrm().Read(doc, "document_id")
		}

		//将图片链接更换成绝对链接
		toc := converter.Toc{
			Id:    doc.DocumentId,
			Pid:   doc.ParentId,
			Title: doc.DocumentName,
			Link:  fmt.Sprintf("%v.html", doc.DocumentId),
		}
		cfg.Toc = append(cfg.Toc, toc)
		//图片处理，如果图片路径不是http开头，则表示是相对路径的图片，加上BaseUrl.如果图片是以http开头的，下载下来
		if gq, err := goquery.NewDocumentFromReader(strings.NewReader(doc.Release)); err == nil {
			gq.Find("img").Each(func(i int, s *goquery.Selection) {
				pic := ""
				if src, ok := s.Attr("src"); ok {
					if srcLower := strings.ToLower(src); strings.HasPrefix(srcLower, "http://") || strings.HasPrefix(srcLower, "https://") {
						pic = src
					} else {
						if utils.StoreType == utils.StoreOss {
							pic = strings.TrimRight(beego.AppConfig.String("oss::Domain"), "/ ") + "/" + strings.TrimLeft(src, "./")
						} else {
							pic = baseUrl + src
						}
					}
					//下载图片，放到folder目录下
					ext := ""
					if picSlice := strings.Split(pic, "?"); len(picSlice) > 0 {
						ext = filepath.Ext(picSlice[0])
					}
					filename := cryptil.Md5Crypt(pic) + ext
					localPic := folder + filename
					req := httplib.Get(pic).SetTimeout(5*time.Second, 5*time.Second)
					if strings.HasPrefix(pic, "https") {
						req.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
					}
					req.Header("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/65.0.3298.4 Safari/537.36")
					if err := req.ToFile(localPic); err == nil { //成功下载图片
						s.SetAttr("src", filename)
					} else {
						beego.Error("错误:", err, filename, pic)
						s.SetAttr("src", pic)
					}

				}
			})
			gq.Find(".markdown-toc").Remove()
			doc.Release, _ = gq.Find("body").Html()
		}

		//生成html
		if htmlStr, err := utils.ExecuteViewPathTemplate("ebook/tpl_export.html", map[string]interface{}{
			"Model":    book,
			"Doc":      doc,
			"BaseUrl":  baseUrl,
			"Nickname": nickname,
			"Date":     cfg.Timestamp,
			"Now":      time.Now().Unix(),
		}); err == nil {
			htmlName := folder + toc.Link
			ioutil.WriteFile(htmlName, []byte(htmlStr), os.ModePerm)
		} else {
			beego.Error(err.Error())
		}
	}

	//复制css文件到目录
	if b, err := ioutil.ReadFile("static/editor.md/css/export-editormd.css"); err == nil {
		ioutil.WriteFile(folder+"editormd.css", b, os.ModePerm)
	} else {
		beego.Error("css样式不存在", err)
	}

	cfgFile := folder + "config.json"
	ioutil.WriteFile(cfgFile, []byte(util.InterfaceToJson(cfg)), os.ModePerm)
	if Convert, err := converter.NewConverter(cfgFile, debug); err == nil {
		// 电子书生成回调
		Convert.Callback = m.callback
		if err := Convert.Convert(); err != nil {
			beego.Error(err.Error())
		}
	} else {
		beego.Error(err.Error())
	}

	Convert, err := converter.NewConverter(cfgFile, debug)
	if err != nil {
		beego.Error(err.Error())
		return
	}

	// 设置电子书生成回调
	Convert.Callback = m.callback
	if err = Convert.Convert(); err != nil {
		beego.Error(err.Error())
	}
}

func (m *Ebook) callback(identify, ebookPath string) {
	var ebook Ebook

	ebookPath = strings.TrimLeft(ebookPath, "./")
	book, err := NewBook().FindByIdentify(identify)
	if err != nil {
		beego.Error(err)
		return
	}
	ext := filepath.Ext(ebookPath)
	o := orm.NewOrm()
	if err = o.QueryTable(m).Filter("book_id", book.BookId).Filter("ext", ext).OrderBy("-id").One(&ebook); err != nil {
		beego.Error(err)
		return
	}
	if ebook.Id == 0 {
		return
	}

	info, err := os.Stat(ebookPath)
	if err != nil {
		beego.Error(err)
		ebook.Status = EBookStatusFailure
		o.Update(&ebook)
		return
	}

	newEbookPath := fmt.Sprintf("projects/%v/books/%v%v", book.Identify, time.Now().Unix(), ext)
	switch utils.StoreType {
	case utils.StoreOss:
		//不要开启gzip压缩，否则会出现文件损坏的情况
		if err := store.ModelStoreOss.MoveToOss(ebookPath, newEbookPath, true, false); err != nil {
			beego.Error(err)
		} else { // 设置下载头
			store.ModelStoreOss.SetObjectMeta(newEbookPath, book.BookName+ext)
		}
	case utils.StoreLocal: //本地存储
		newEbookPath = "uploads/" + newEbookPath
		if err = store.ModelStoreLocal.MoveToStore(ebookPath, newEbookPath); err != nil {
			beego.Error(err)
		}
	}

	ebook.Size = info.Size()
	ebook.Path = "/" + newEbookPath
	ebook.Status = EBookStatusSuccess
	o.Update(&ebook)
}
