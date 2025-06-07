#!/usr/bin/env bash
[[ $1 == -stderr ]] && exec 1>&2

echo "I am a test"
echo -e -n "This ends with a return\r"
echo -e "Windows style\r\nNext after CRLF"
printf "split write one "
printf "plus two\n"
printf "alpha\nbeta\ngamma\n"
printf "Null byte \x00 inside\n"
sleep 0.1
echo "Emoji 😀 test"
sleep 0.1
printf "dangling line without newline"
sleep 0.2
