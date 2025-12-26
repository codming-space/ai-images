#!/bin/bash

# 定义源目录和目标目录
SOURCE_DIR=~/Project/new-api-upstream
TARGET_DIR=~/Project/ai-images/new-api/v0.10.4

# 展开波浪号
SOURCE_DIR="${SOURCE_DIR/#\~/$HOME}"
TARGET_DIR="${TARGET_DIR/#\~/$HOME}"

# 检查源目录是否存在
if [ ! -d "$SOURCE_DIR" ]; then
    echo "错误: 源目录不存在: $SOURCE_DIR"
    exit 1
fi

# 检查目标目录是否存在，不存在则创建
if [ ! -d "$TARGET_DIR" ]; then
    echo "目标目录不存在，正在创建: $TARGET_DIR"
    mkdir -p "$TARGET_DIR"
fi

# 进入源目录
cd "$SOURCE_DIR" || exit 1

# 检查是否是 git 仓库
if [ ! -d .git ]; then
    echo "错误: $SOURCE_DIR 不是一个 git 仓库"
    exit 1
fi

echo "正在查找修改过的文件..."

# 获取所有修改过的文件（包括已暂存和未暂存的）
# --exclude-standard: 排除 .gitignore 中的文件
MODIFIED_FILES=$(git ls-files --modified --others --exclude-standard)

if [ -z "$MODIFIED_FILES" ]; then
    echo "没有发现修改过的文件"
    exit 0
fi

echo "找到以下修改过的文件:"
echo "$MODIFIED_FILES"
echo ""

# 复制文件
COPIED_COUNT=0
while IFS= read -r file; do
    if [ -f "$file" ]; then
        # 获取文件的目录路径
        file_dir=$(dirname "$file")
        
        # 在目标目录中创建相应的目录结构
        target_file_dir="$TARGET_DIR/$file_dir"
        mkdir -p "$target_file_dir"
        
        # 复制文件
        cp "$file" "$TARGET_DIR/$file"
        echo "✓ 已复制: $file"
        ((COPIED_COUNT++))
    fi
done <<< "$MODIFIED_FILES"

echo ""
echo "完成! 共复制了 $COPIED_COUNT 个文件到 $TARGET_DIR"
