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
		} else if strings.HasPrefix(lines[i], "INSERT INTO `transaction_evidences` ") {
			builder.WriteString(changeDBSchema(lines[i], prefixBeforeRemoveTransactionEvidenceColumns, prefixAfterRemoveTransactionEvidenceColumns))
		} else if strings.HasPrefix(lines[i], "INSERT INTO `shippings` ") {
			builder.WriteString(changeDBSchema(lines[i], prefixBeforeRemoveShippingColumns, prefixAfterRemoveShippingColumns))
		} else {
			builder.WriteString(lines[i])
		}
		builder.WriteString("\n")
	}
	output := builder.String()
	ioutil.WriteFile("./initial2.sql", []byte(output), 0666)
}

const prefixBeforeAddParentCategoryIds = "INSERT INTO `items` (`id`,`seller_id`,`buyer_id`,`status`,`name`,`price`,`description`,`image_name`,`category_id`,`created_at`,`updated_at`) VALUES "
const prefixAfterAddParentCategoryIds = "INSERT INTO `items` (`id`,`seller_id`,`buyer_id`,`status`,`name`,`price`,`description`,`image_name`,`category_id`,`parent_category_id`,`created_at`,`updated_at`) VALUES "

const prefixBeforeRemoveTransactionEvidenceColumns = "INSERT INTO `transaction_evidences` (`id`,`seller_id`,`buyer_id`,`status`,`item_id`,`item_name`,`item_price`,`item_description`,`item_category_id`,`item_root_category_id`,`created_at`,`updated_at`) VALUES "
const prefixAfterRemoveTransactionEvidenceColumns = "INSERT INTO `transaction_evidences` (`id`, `seller_id`, `buyer_id`, `status`, `item_id`) VALUES "

const prefixBeforeRemoveShippingColumns = "INSERT INTO `shippings` (`transaction_evidence_id`,`status`,`item_name`,`item_id`,`reserve_id`,`reserve_time`,`to_address`,`to_name`,`from_address`,`from_name`,`img_binary`,`created_at`,`updated_at`) VALUES "
const prefixAfterRemoveShippingColumns = "INSERT INTO `shippings` (`transaction_evidence_id`, `status`, `reserve_id`, `reserve_time`, `img_binary`) VALUES "

type SchemaKey struct {
	before int
	after int
}

func changeDBSchema(line, prefixBefore, prefixAfter string) string {
	line = strings.TrimPrefix(line, prefixBefore)
	itemStrings := strings.SplitAfter(line, "), ")

	beforeKeys := getKeys(prefixBefore)
	afterKeys := getKeys(prefixAfter)

	keyIndexes := []SchemaKey{}
	for beforeIndex, beforeKey := range beforeKeys {
		for afterIndex, afterKey := range afterKeys {
			if afterKey == beforeKey {
				keyIndexes = append(keyIndexes, SchemaKey{
					before: beforeIndex,
					after: afterIndex,
				})
				break
			}
		}
	}

	var builder strings.Builder
	builder.WriteString(prefixAfter)

	itemLen := len(itemStrings)
	for i, str := range itemStrings {
		values := getKeys(str)
		afterValue := make([]string, len(keyIndexes))
		for valueIndex, value := range values {
			for _, keyIndex := range keyIndexes {
				if valueIndex == keyIndex.before {
					afterValue[keyIndex.after] = value
					break
				}
			}
		}
		builder.WriteString("(")

		for vI, vV := range afterValue {
			builder.WriteString(vV)
			if vI + 1 != len(keyIndexes) {
				builder.WriteString(", ")
			}
		}

		builder.WriteString(strings.Join(afterValue, ", "))
		if i + 1 == itemLen {
			builder.WriteString(")")
		} else {
			builder.WriteString("), ")
		}
	}

	return builder.String()
}

func addParentCategoryIds(line string) string {
	line = strings.TrimPrefix(line, prefixBeforeAddParentCategoryIds)
	itemStrings := strings.SplitAfter(line, "), ")

	var builder strings.Builder
	builder.WriteString(prefixAfterAddParentCategoryIds)
	for i := range itemStrings {
		builder.WriteString(addParentCategoryId(itemStrings[i]))
	}
	return builder.String()
}


func getKeys(str string) []string {
	return strings.Split(getKakkoContent(strings.ReplaceAll(str, " ", "")), ",")
}

func getKakkoContent(str string) string {
	return str[strings.Index(str, "("):strings.LastIndex(str, ")") - 1]
	return strings.Split(strings.Split(str, "(")[1], ")")[0]
}

var categoryRe = regexp.MustCompile(`\.jpg', (\d+), `)

func addParentCategoryId(itemString string) string {
	categoryID := categoryRe.FindStringSubmatch(itemString)[1]
	parentCategoryID := getParentCategory(categoryID)
	return categoryRe.ReplaceAllString(itemString, ".jpg', " + categoryID + ", " + parentCategoryID + ", ")
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
