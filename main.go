package main

import (
	"fmt"
	"sync"
)

// User represents a user in the system
type User struct {
	Name       string
	PhoneNum   string
	Emails     map[int]string 
}

func (u *User)readEmails(wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 0; i < 100; i++ {
		fmt.Println(u.Emails[i])
	}
}

func (u *User) setEmails(wg *sync.WaitGroup) {
	defer wg.Done()
	for i := 0; i < 100; i++ {
		u.Emails[i] = fmt.Sprintf("email%d@example.com", i)
	}
}

func (u *User) processUserPhoneNumbers() {
	var wg sync.WaitGroup
	wg.Add(2)

	go u.setEmails(&wg)

	go u.readEmails(&wg)

	wg.Wait()
}

func add(x int, y int) int {
	return x + y
}

func main() {

	u := &User{
		Name:       "Alice",
		PhoneNum:"1234567",
		Emails:     make(map[int]string),
	}
	u.processUserPhoneNumbers()
	fmt.Println(add(5, 3))
}