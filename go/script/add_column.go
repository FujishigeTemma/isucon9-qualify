package sub

import (
	"io/ioutil"
	"regexp"
	"strings"
)

func main() {
	bytes, err := ioutil.ReadFile("./initial.sql")
	if err != nil {
		panic(err)
	}

	body := string(bytes)
	lines := strings.Split(body, "\n")

	var builder strings.Builder
	for i := range lines {
		if strings.HasPrefix(lines[i], "INSERT INTO `items` ") {
			builder.WriteString(addParentCategoryIds(lines[i]))
		} else {
			builder.WriteString(lines[i])
		}
		builder.WriteString("\n")
	}
	output := builder.String()
	ioutil.WriteFile("./initial2.sql", []byte(output), 0666)
}

const prefix = "INSERT INTO `items` (`id`,`seller_id`,`buyer_id`,`status`,`name`,`price`,`description`,`image_name`,`category_id`,`created_at`,`updated_at`) VALUES "
var prefixRe = regexp.MustCompile(``)

func addParentCategoryIds(line string) string {
	line = strings.TrimPrefix(line, prefix)
	itemStrings := strings.SplitAfter(line, "), ")

	var builder strings.Builder
	builder.WriteString(prefix)
	for i := range itemStrings {
		builder.WriteString(addParentCategoryId(itemStrings[i]))
	}
	return builder.String()
}

var categoryRe = regexp.MustCompile(`\.jpg', (\d+), `)

func addParentCategoryId(itemString string) string {
	categoryID := categoryRe.FindStringSubmatch(itemString)[1]
	parentCategoryID := getParentCategory(categoryID)
	return categoryRe.ReplaceAllString(itemString, ".jpg, " + categoryID + ", " + parentCategoryID + ", ")
}

func getParentCategory(categoryID string) string {
	chars := []rune(categoryID)
	if len(chars) > 1 {
		if chars[1] == '0' {
			return "0"
		}
		return string(chars[0]) + "0"
	}
	if chars[0] == '1' {
		return "0"
	}
	return "1"
}
