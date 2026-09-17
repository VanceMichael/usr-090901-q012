package domain

// MaskFor 返回某角色的字段裁剪规则；未知角色按最严格处理。
func (r *Rules) MaskFor(role string) FieldMask {
	if m, ok := r.Masking[role]; ok {
		return m
	}
	return FieldMask{Amount: "hidden", Contact: "hidden"}
}

// AmountBandFor 把金额（分）映射到夹具分档标签。
func (r *Rules) AmountBandFor(cents int64) string {
	label := ""
	for _, b := range r.AmountBands {
		if cents >= b.MinCents {
			label = b.Label
		}
	}
	return label
}

// MaskContact 按模式裁剪联系方式：full 原样、masked 保留前4后2、hidden 置空。
func MaskContact(mode, contact string) string {
	if contact == "" {
		return ""
	}
	switch mode {
	case "full":
		return contact
	case "masked":
		r := []rune(contact)
		if len(r) <= 6 {
			return "****"
		}
		return string(r[:4]) + "****" + string(r[len(r)-2:])
	default:
		return ""
	}
}
