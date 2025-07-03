package main

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
)

func GetInstanceAuthToken(instance *WrapperInstance) (string, string, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("data/wrapper/rootfs/data/instances/%s/mpl_db/cookies.sqlitedb", instance.Id))
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	dsidQuery := "select value from cookies where name == \"X-Dsid\";"
	result, err := db.Query(dsidQuery)
	if err != nil {
		return "", "", err
	}
	var dsid string
	for result.Next() {
		err = result.Scan(&dsid)
		if err != nil {
			return "", "", err
		}
	}
	tokenQuery := fmt.Sprintf("select value from cookies where name == \"mz_at_ssl-%s\";", dsid)
	result, err = db.Query(tokenQuery)
	if err != nil {
		return "", "", err
	}
	var token string
	for result.Next() {
		err = result.Scan(&token)
		if err != nil {
			return "", "", err
		}
	}
	return dsid, token, nil
}

func GetMusicToken(instance *WrapperInstance) (string, error) {
	token, err := os.ReadFile(fmt.Sprintf("data/wrapper/rootfs/data/instances/%s/MUSIC_TOKEN", instance.Id))
	if err != nil {
		return "", err
	}
	return string(token), nil
}

func GetToken() (string, error) {
	req, err := http.NewRequest("GET", "https://beta.music.apple.com", nil)
	if err != nil {
		return "", err
	}

	resp, err := GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex := regexp.MustCompile(`/assets/index-legacy-[^/]+\.js`)
	indexJsUri := regex.FindString(string(body))

	req, err = http.NewRequest("GET", "https://beta.music.apple.com"+indexJsUri, nil)
	if err != nil {
		return "", err
	}

	resp, err = GetHttpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			panic(err)
		}
	}(resp.Body)

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	regex = regexp.MustCompile(`eyJh([^"]*)`)
	token := regex.FindString(string(body))

	return token, nil
}
