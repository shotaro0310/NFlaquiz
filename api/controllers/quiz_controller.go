package controllers

import (
  "time"
  "sort"
	"errors"
	"net/http"
	"strconv"
  "math/rand"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/gin-gonic/gin"
	"github.com/raylicola/NFlaquiz/database"
	"github.com/raylicola/NFlaquiz/models"
	"github.com/raylicola/NFlaquiz/utils"
	"gorm.io/gorm"
)

// 検索条件に当てはまるクイズを10問選ぶ
// ヒット数が10個以下なら全て選択する
// 受信：
//   colors(array): 選択された色
//    areas(array): 選択された地域
//   bookmark(int): ブックマークで絞り込むか否か
// 戻り値：
//    10問以下のクイズセット
func SelectQuiz(c *gin.Context) {

  // 絞り込み条件を取得
  var req models.QuizFilter
  if err := c.Bind(&req); err != nil {
    c.JSON(http.StatusBadRequest, err)
    return
  }

  colors := req.Colors
  areas := req.Areas
  bookmark, _ := strconv.Atoi(req.Bookmark)
  user, err := utils.AuthUser(c)

  var countries []models.Country
  var flag_colors []models.FlagColor
  var results []models.Result

  // まずは全ての国を候補とする
  options := mapset.NewSet[string]()
  database.DB.Find(&countries)
  for _, v := range countries {
    options.Add(v.ID)
  }

  // 該当地域の国を絞る
  if len(areas) != 0 {
    database.DB.Where("area_id in (?)", areas).Find(&countries)
    selected := mapset.NewSet[string]()
    for _, v := range countries {
      selected.Add(v.ID)
    }
    options = options.Intersect(selected)
  }

  // 国旗の色が完全一致する国を絞る
  if len(colors) != 0 {
    database.DB.Select("country_id").Where("color_id in (?)", colors).
      Group("country_id").Having("count(*) = ?", len(colors)).Find(&flag_colors)
    selected := mapset.NewSet[string]()
    for _, v := range flag_colors {
      selected.Add(v.CountryID)
    }
    options = options.Intersect(selected)
  }

  // ユーザーがブックマークした国を絞る
  if  (err == nil) && (bookmark == 1) {
    database.DB.Where("user_id=?", user.ID).Where("bookmark=?", bookmark).Find(&results)
    selected := mapset.NewSet[string]()
    for _, v := range results {
      selected.Add(v.CountryID)
    }
    options = options.Intersect(selected)
  }

  // 検索のための型変換
  var country_array []string
  options.Each(func(country string) bool {
    country_array = append(country_array, country)
    return false
  })

  // 絞り込んだ国からクイズを選択
  var quizzes []models.Quiz

  if err != nil {
    // 未ログインユーザーの場合, ランダムにクイズを選択
    database.DB.Where("quizzes.country_id in (?)", country_array).
      Order("rand()").Limit(10).Find(&quizzes)
  } else {
    // ログイン済みユーザーの場合, 未回答のものを優先して選択
    query := database.DB.Joins("left outer join results on quizzes.country_id = results.country_id").
      Where("quizzes.country_id in (?)", country_array).
      Where("quizzes.country_id not in (?)", database.DB.Table("results").Select("country_id").Where("user_id=?", user.ID)).
      Order("rand()").Limit(10).Find(&quizzes)

    // 未回答問題数が10未満の場合, 正答率が低い順に残りを埋める
    left := int(10 - query.RowsAffected)
    if left > 0 {
      var low_weight_quizzes []models.Quiz
      low_weight_query := database.DB.Joins("left outer join results on quizzes.country_id = results.country_id").
        Where("quizzes.country_id in (?)", country_array).
        Where("user_id = ?", user.ID).
        Order("weight").Limit(left).Find(&low_weight_quizzes)

      database.DB.Raw(
				"(?) UNION (?)",
				query,
				low_weight_query,
			).Scan(&quizzes)
    }
  }

  // 選択肢の生成
  database.DB.Find(&countries)
  rand.Seed(time.Now().UnixNano())
  var country_names []string
  var name_options [][]string

  for _, country := range countries {
    country_names = append(country_names, country.Name)
  }

  for i := 0; i < len(quizzes); i++ {
    var tmp []string = []string{quizzes[i].CountryName}

    for j := 0; j < 3; j++ {
      for {
        var country_name = country_names[rand.Intn(len(country_names))]
        if utils.Contains(tmp, country_name) == false {
          tmp = append(tmp, country_name)
          break
        }
      }
    }

    sort.Slice(tmp, func(i, j int) bool {
      return tmp[i] < tmp[j]
    })

    name_options = append(name_options, tmp)
  }

  var quiz_with_options []models.QuizWithOption
  for  i, quiz := range quizzes {
    quiz_with_options = append(quiz_with_options, models.QuizWithOption{
      ID: quiz.ID,
      CountryName: quiz.CountryName,
      CountryID: quiz.CountryID,
      Hint1: quiz.Hint1,
      Hint2: quiz.Hint2,
      Hint3: quiz.Hint3,
      Options: name_options[i],
    })
  }

  c.JSON(http.StatusOK, gin.H{"quizzes": quiz_with_options})
}

// クイズの回答状況をもとにResultを更新する
// ログインしているときのみ
// 受信：
//   [{country_id: 国ID, answer: (0|1), bookmark: (0|1)}, ...]
//     answer -> 1:正解, 0: 不正解
//   bookmark -> 1:登録する, 0:しない（既に登録済みであれば変更しない）
func UpdateResult(c *gin.Context) {

	// データをバインド
	var req []models.AnswerStatus
	err := c.BindJSON(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"err_msg": "データのバインドに失敗しました"})
		return
	}

	// ユーザーが認証されていない場合
	user, err := utils.AuthUser(c)
	if err != nil {
    c.JSON(http.StatusUnauthorized, gin.H{"err_msg": "認証されていません"})
		return
	}

	// resultテーブル更新処理
	for _, v := range req {

		var result models.Result
		query := database.DB.Where("country_id=?", v.CountryID).Where("user_id=?", user.ID).First(&result)

		// 重みは[0.25, 1.0]で正誤により0.25ずつ調整する
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			// 初回答のクイズの場合はResultを新規作成
			weight := 1.0
			if v.Answer == 0 {
				weight = 0.25
			}
			new_result := models.Result{
				CountryID: v.CountryID,
				UserID: user.ID,
				Weight: weight,
				Bookmark: v.Bookmark,
			}
			database.DB.Create(&new_result)

		} else {
			// 過去に回答したことのあるクイズの場合はResultを更新
			weight := result.Weight
			if v.Answer == 1 && result.Weight <= 0.75{
				weight += 0.25
			} else if v.Answer == 0 && result.Weight >= 0.5 {
				weight -= 0.25
			}

			database.DB.Model(&result).Where("user_id=?", user.ID).Where("country_id=?", v.CountryID).Update("weight", weight)

			// ブックマークの更新
			if v.Bookmark == 1 {
				database.DB.Model(&result).Where("user_id=?", user.ID).Where("country_id=?", v.CountryID).Update("bookmark", 1)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{})
}