package main

import (
	"context"
	"testing"

	"github.com/mmcdole/gofeed"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type modifierFuncTest struct {
	key    string
	value  string
	expect gofeed.Item
}

func TestCreateModifierInvalidKey(t *testing.T) {
	t.Parallel()

	modifiersMap := CreateModifierMap()

	f := CreateModifier("invalidKey", "value", modifiersMap)
	if f != nil {
		t.Fail()
	}
}

func TestCreateModifier(t *testing.T) {
	t.Parallel()

	modifiersMap := CreateModifierMap()
	testItem := createTestItem()

	expectRemoveDescription := *testItem
	expectRemoveDescription.Description = ""
	expectRemoveContent := *testItem
	expectRemoveContent.Content = ""

	for _, tt := range []modifierFuncTest{
		// remove
		{key: "rm.description", value: "", expect: expectRemoveDescription},
		{key: "rm.content", value: "", expect: expectRemoveContent},
	} {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()

			f := CreateModifier(tt.key, tt.value, modifiersMap)
			result := f(testItem)
			assert.Check(t, is.DeepEqual(tt.expect, *result))
		})
	}
}

func TestCreateModifierWithNonExistentField(t *testing.T) {
	t.Parallel()

	modifiersMap := CreateModifierMap()
	testItem := createTestItem()

	f := CreateModifier("rm.nonexistent", "", modifiersMap)
	assert.Assert(t, f == nil)

	// check that the item is not modified
	modifiers := []ModifierFunc{}
	if f != nil {
		modifiers = append(modifiers, f)
	}

	result, err := Apply(context.Background(), &gofeed.Feed{Items: []*gofeed.Item{testItem}}, nil, modifiers)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(testItem, result.Items[0]))
}
