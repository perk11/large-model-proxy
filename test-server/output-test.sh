#!/usr/bin/env sh
[ "$1" = "-stderr" ] && exec 1>&2

echo "I am a test"
printf "This ends with a return\r"
printf "Windows style\r\nNext after CRLF\n"
printf "split write one "
printf "plus two\n"
printf "alpha\nbeta\ngamma\n"
printf "Null byte \000 inside\n"
sleep 0.1
echo "Emoji 😀 test"
sleep 0.1
printf "dangling line without newline"
sleep 0.2
