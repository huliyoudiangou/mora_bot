// dbcheck：临时诊断工具，只读查询线上数据库状态。
package main

import (
	"fmt"
	"os"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"mora_bot/internal/db"
)

func main() {
	path := "data/mora_bot.db"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	gdb, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		fmt.Println("open err:", err)
		os.Exit(1)
	}

	fmt.Println("=== system_configs ===")
	var cfgs []db.SystemConfig
	gdb.Find(&cfgs)
	for _, c := range cfgs {
		fmt.Printf("%s = %s\n", c.Key, c.Value)
	}

	fmt.Println("\n=== users ===")
	var users []db.User
	gdb.Find(&users)
	for _, u := range users {
		fmt.Printf("tg=%d name=%s jf=%s status=%s\n", u.TelegramID, u.DisplayName(), u.JellyfinUsername, u.Status)
	}

	fmt.Println("\n=== drama_requests (最近20) ===")
	var reqs []db.DramaRequest
	gdb.Order("id desc").Limit(20).Find(&reqs)
	for _, r := range reqs {
		fmt.Printf("#%d user=%d title=%q link=%q status=%s created=%s\n", r.ID, r.UserID, r.Title, r.Link, r.Status, r.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	var cnt int64
	gdb.Model(&db.DramaRequest{}).Count(&cnt)
	fmt.Printf("total drama_requests: %d\n", cnt)

	fmt.Println("\n=== sign_in_records (最近5) ===")
	var signs []db.SignInRecord
	gdb.Order("id desc").Limit(5).Find(&signs)
	for _, s := range signs {
		fmt.Printf("user=%d day=%s streak=%d\n", s.UserID, s.SignDay, s.Streak)
	}
}
