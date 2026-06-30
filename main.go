package main

import (
	"log"
	"net/http"
	"time"

	"seat-killer/booker"
	"seat-killer/config"
	"seat-killer/mapper"
	"seat-killer/retry"
	"seat-killer/sso"
	"seat-killer/user"
)

const (
	// Total duration of the booking window after the official booking time.
	bookingWindow = 60 * time.Second
	// Duration to wait after receiving a real (non-rate-limit) response,
	// matching the server's ~3s per-account cooldown with a small margin.
	cooldownDuration = 3500 * time.Millisecond
	// Polling interval used to detect cooldown expiration after a
	// "超出可预约座位时间范围" response. "请求太频繁" responses do NOT
	// reset the server cooldown timer, so rapid polling is safe and lets
	// us break through the cooldown at the earliest possible moment.
	rapidPollInterval = 200 * time.Millisecond
)

func main() {
	log.Println("Starting Seat Killer...")

	// --- 1. Load Configs & Map ---
	userInfo, err := config.LoadUserInfo("user_info.yml")
	if err != nil {
		log.Fatalf("Failed to load user_info.yml: %v", err)
	}
	seatCfg, err := config.LoadSeatConfig("user_config.yml")
	if err != nil {
		log.Fatalf("Failed to load user_config.yml: %v", err)
	}
	if _, err = mapper.LoadSeatMap("seat_report.txt"); err != nil {
		log.Fatalf("Failed to load seat map: %v", err)
	}
	log.Println("Configs and seat map loaded.")
	log.Printf("Loaded user config for SchoolID: %s", userInfo.SchoolID)

	// --- 2. Determine Today's Booking Task ---
	weekdayMap := map[time.Weekday]string{
		time.Sunday: "周日", time.Monday: "周一", time.Tuesday: "周二",
		time.Wednesday: "周三", time.Thursday: "周四", time.Friday: "周五",
		time.Saturday: "周六",
	}
	todayWeekdayStr := weekdayMap[time.Now().Weekday()]
	dayConfig, ok := seatCfg.WeekConfig[todayWeekdayStr]
	if !ok || !dayConfig.Enable || len(dayConfig.Seats) == 0 {
		log.Printf("Booking is not enabled for today (%s) or no seats configured. Exiting.", todayWeekdayStr)
		return
	}
	log.Printf("Found booking task for today (%s): Run at %d:%02d to book one of %d seat(s).",
		todayWeekdayStr, dayConfig.RunAtHour, dayConfig.RunAtMinute, len(dayConfig.Seats))

	bookingDay := time.Now().AddDate(0, 0, 2)
	targetTime := time.Date(bookingDay.Year(), bookingDay.Month(), bookingDay.Day(), dayConfig.BookStartHour, 0, 0, 0, time.Local)
	log.Printf("Task for SchoolID [%s]: Booking for %s, from %s for %d hours. Seats: %v",
		userInfo.SchoolID,
		targetTime.Format("2006-01-02"),
		targetTime.Format("15:04"),
		dayConfig.Duration,
		dayConfig.Seats)

	// --- 3. Login NOW (well before the booking window) ---
	log.Println("Logging in...")
	var client *http.Client
	loginFunc := func() error {
		var loginErr error
		client, _, loginErr = sso.Login(userInfo.SchoolID, userInfo.Password)
		return loginErr
	}
	if err := retry.WithRetry(loginFunc, 20, 3*time.Second); err != nil {
		log.Fatalf("Login failed: %v. Please check your user_info.yml.", err)
	}
	loggedInUser, err := user.GetUserInfo(client)
	if err != nil {
		log.Fatalf("User info fetch failed: %v", err)
	}
	log.Printf("Logged in as SchoolID [%s] (UID: %s). Waiting for booking window...",
		userInfo.SchoolID, loggedInUser.UID)

	// --- 4. Wait until exactly the official booking time ---
	now := time.Now()
	officialBookTime := time.Date(now.Year(), now.Month(), now.Day(), dayConfig.RunAtHour, dayConfig.RunAtMinute, 0, 0, time.Local)
	endTime := officialBookTime.Add(bookingWindow)

	log.Printf("Booking window: %s -> %s", officialBookTime.Format("15:04:05"), endTime.Format("15:04:05"))

	if time.Now().After(endTime) {
		log.Println("Booking window has already passed. Exiting.")
		return
	}

	// Sleep until ~100ms before the target, then busy-wait for precision.
	if sleepDuration := time.Until(officialBookTime) - 100*time.Millisecond; sleepDuration > 0 {
		time.Sleep(sleepDuration)
	}
	for time.Now().Before(officialBookTime) {
		// busy-wait for precise timing
	}

	// Delay the first request to compensate for client-server clock drift.
	// The server consistently opens slightly after the client's 20:00:00,
	// so sending at T+delay avoids the "超出时间范围" rejection that would
	// waste a critical 3s cooldown cycle.
	time.Sleep(time.Duration(seatCfg.Global.FirstRequestDelayMs) * time.Millisecond)

	// --- 5. Execute Precision Booking ---
	log.Printf("--- Booking window opened for SchoolID [%s]: Trying %d seats with adaptive cooldown ---",
		userInfo.SchoolID, len(dayConfig.Seats))

	if success, seat := executePrecisionBooking(client, userInfo, loggedInUser, &dayConfig, endTime); success {
		bookTime := time.Date(bookingDay.Year(), bookingDay.Month(), bookingDay.Day(), dayConfig.BookStartHour, 0, 0, 0, time.Local)
		log.Printf("BOOKING SUCCESSFUL for SchoolID [%s]! Seat '%s' in room '%s' booked for %s from %s for %d hours.",
			userInfo.SchoolID,
			seat,
			dayConfig.Name,
			bookTime.Format("2006-01-02"),
			bookTime.Format("15:04"),
			dayConfig.Duration)
		return
	}

	log.Println("Seat Killer finished: all attempts failed within the booking window.")
}

// executePrecisionBooking sends one request at a time with adaptive cooldown,
// respecting the server's ~3s rate-limit window per real response.
// Occupied seats are permanently removed to avoid wasting cooldown cycles.
func executePrecisionBooking(client *http.Client, cfgUser *config.UserInfo, loggedInUser *user.UserInfo, dayCfg *config.DayConfig, endTime time.Time) (bool, string) {
	// Copy the seat list so we can remove occupied ones.
	availableSeats := make([]string, len(dayCfg.Seats))
	copy(availableSeats, dayCfg.Seats)

	bookingDay := time.Now().AddDate(0, 0, 2)
	bookTime := time.Date(bookingDay.Year(), bookingDay.Month(), bookingDay.Day(), dayCfg.BookStartHour, 0, 0, 0, time.Local)
	duration := time.Duration(dayCfg.Duration) * time.Hour

	seatIndex := 0

	for len(availableSeats) > 0 && time.Now().Before(endTime) {
		if seatIndex >= len(availableSeats) {
			seatIndex = 0
		}
		seatNum := availableSeats[seatIndex]

		seatID, err := mapper.GetSeatID(dayCfg.Name, seatNum)
		if err != nil {
			log.Printf("Cannot find seat '%s' in room '%s', removing.", seatNum, dayCfg.Name)
			availableSeats = removeIndex(availableSeats, seatIndex)
			continue
		}

		result, bookErr := sendBookingRequest(client, cfgUser, loggedInUser, dayCfg.Name, seatNum, seatID, bookTime, duration)

		// Network or parsing error — short wait, retry same seat.
		if bookErr != nil {
			log.Printf("Network error for seat '%s': %v", seatNum, bookErr)
			time.Sleep(1 * time.Second)
			continue
		}

		if result.IsSuccess() {
			return true, seatNum
		}

		if result.IsRateLimited() {
			// Unexpected with clean state; short wait, retry same seat.
			time.Sleep(rapidPollInterval)
			continue
		}

		if result.IsSeatOccupied() {
			log.Printf("Seat '%s' is occupied, removing from candidates.", seatNum)
			availableSeats = removeIndex(availableSeats, seatIndex)
			// Rapid-poll through the cooldown; if the next real response
			// happens to be a success, propagate it immediately.
			if ok, seat := rapidPollUntilReal(client, cfgUser, loggedInUser, dayCfg, &availableSeats, &seatIndex, bookTime, duration, endTime); ok {
				return true, seat
			}
			continue
		}

		if result.IsTimeRangeError() {
			// Server hasn't opened the window yet. The "超出时间范围"
			// response triggered a ~3s cooldown. Rapid-poll to break
			// through the cooldown at the earliest possible moment,
			// then the main loop retries the same seat.
			log.Println("Server window not open yet, rapid-polling until cooldown expires...")
			if ok, seat := rapidPollUntilReal(client, cfgUser, loggedInUser, dayCfg, &availableSeats, &seatIndex, bookTime, duration, endTime); ok {
				return true, seat
			}
			continue
		}

		// Other server rejection — wait cooldown, rotate to next seat.
		seatIndex++
		time.Sleep(cooldownDuration)
	}

	if len(availableSeats) == 0 {
		log.Println("All candidate seats have been taken by others.")
	}
	return false, ""
}

// sendBookingRequest sends a single booking request and logs the attempt/result.
func sendBookingRequest(client *http.Client, cfgUser *config.UserInfo, loggedInUser *user.UserInfo, roomName, seatNum string, seatID int, bookTime time.Time, duration time.Duration) (*booker.BookResponseData, error) {
	log.Printf("Attempting to book for SchoolID [%s]: room '%s', seat '%s' (%d)",
		cfgUser.SchoolID, roomName, seatNum, seatID)

	result, err := booker.BookSeat(&booker.BookingRequest{
		Client:    client,
		UserID:    loggedInUser.UID,
		SeatID:    seatID,
		BeginTime: bookTime,
		Duration:  duration,
	})
	if err != nil {
		return nil, err
	}

	log.Printf("Booking result for SchoolID [%s]: [%v] %s",
		cfgUser.SchoolID, result.CODE, result.MESSAGE)
	return result, nil
}

// rapidPollUntilReal polls at short intervals after a real response (e.g.
// "超出时间范围" or "座位已被占用") to break through the cooldown ASAP.
// It cycles through ALL available seats so that the first request to pierce
// the cooldown is an effective booking attempt, regardless of which seat
// it happens to land on.
func rapidPollUntilReal(client *http.Client, cfgUser *config.UserInfo, loggedInUser *user.UserInfo, dayCfg *config.DayConfig, availableSeats *[]string, seatIndex *int, bookTime time.Time, duration time.Duration, endTime time.Time) (bool, string) {
	for time.Now().Before(endTime) {
		time.Sleep(rapidPollInterval)

		if len(*availableSeats) == 0 {
			return false, ""
		}
		if *seatIndex >= len(*availableSeats) {
			*seatIndex = 0
		}
		seatNum := (*availableSeats)[*seatIndex]

		seatID, err := mapper.GetSeatID(dayCfg.Name, seatNum)
		if err != nil {
			*availableSeats = removeIndex(*availableSeats, *seatIndex)
			continue
		}

		result, bookErr := sendBookingRequest(client, cfgUser, loggedInUser, dayCfg.Name, seatNum, seatID, bookTime, duration)
		if bookErr != nil {
			// Network error — rotate to next seat and retry.
			*seatIndex = (*seatIndex + 1) % len(*availableSeats)
			continue
		}

		if result.IsSuccess() {
			return true, seatNum
		}
		if result.IsRateLimited() {
			// Cooldown still active — rotate to next seat and keep polling.
			*seatIndex = (*seatIndex + 1) % len(*availableSeats)
			continue
		}
		// Got a real response — cooldown is over.
		if result.IsSeatOccupied() {
			log.Printf("Seat '%s' is occupied, removing from candidates.", seatNum)
			*availableSeats = removeIndex(*availableSeats, *seatIndex)
			// Don't advance seatIndex; next seat slides into this position.
		}
		// Return to the main loop for further handling.
		return false, ""
	}
	return false, ""
}

// removeIndex removes the element at index i from a string slice, preserving order.
func removeIndex(s []string, i int) []string {
	return append(s[:i], s[i+1:]...)
}
