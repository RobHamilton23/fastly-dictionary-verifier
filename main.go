package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/fastly/go-fastly/v3/fastly"
)

var FastlySiteIDs = []string{
	"6cecXOA5eq1mdycR8IETIO", // fe1
	"6wd67qj6gjWStoHWt9QqLM", // fe2
	"7ASqNxevWrE186HznHoMeq", // fe3
	"7LUFSHwH7rvhe3nX3PX61e", // fe4
	"7WBIxgsYSoSNGi0NZEi4ge", // GCDN-Canary
}

type DictionaryRecord struct {
	Hostname string `json:"item_key"`
	SiteID   string `json:"item_value"`
	service  *fastly.Service
}

func main() {
	fastlyClient, err := fastly.NewClient(os.Getenv("FASTLY_API_KEY"))
	if err != nil {
		log.Fatalf("Unable to create fastly client: %v", err)
	}

	serviceChannel := make(chan *fastly.Service)
	dictionaryRecordChannel := make(chan *DictionaryRecord)

	serviceWaitGroup := sync.WaitGroup{}
	serviceWaitGroup.Add(1)

	assertMatchWaitGroup := sync.WaitGroup{}

	// Goroutine that iterates over services as they come in on the serviceChannel,
	// fetches the dictionary entries from that service, then sends them to the
	// dictionaryEntryChannel for further processing
	go func(serviceChannel <-chan *fastly.Service, fastlyClient *fastly.Client) {
		for service := range serviceChannel {
			log.Printf("Received service: %v", service.Name)
			GetDictionaryItems(fastlyClient, service, dictionaryRecordChannel)
		}
		serviceWaitGroup.Done()
		log.Print("Done receiving services")
	}(serviceChannel, fastlyClient)

	// Goroutine that receives dictionaryRecords and verifies their site IDs against
	// policy docs.
	go func(dictionaryRecordChannel <-chan *DictionaryRecord) {
		for dictionaryRecord := range dictionaryRecordChannel {
			assertMatchWaitGroup.Add(1)
			go func(dictionaryEntry *DictionaryRecord) {
				AssertSiteIDMatches(dictionaryEntry)
				assertMatchWaitGroup.Done()
			}(dictionaryRecord)
		}
	}(dictionaryRecordChannel)

	// Start the process by fetching services into the serviceChannel
	GetServices(fastlyClient, serviceChannel)

	// Wait for the serviceChannel to close which happens after all services are fetched
	serviceWaitGroup.Wait()

	// Wait for all dictionary entries to be checked against policy docs
	assertMatchWaitGroup.Wait()

	// Close the dictionaryRecordChannel allowing the goroutine to complete successfully
	close(dictionaryRecordChannel)
}

func GetServices(client *fastly.Client, serviceChan chan<- *fastly.Service) {
	for _, serviceID := range FastlySiteIDs {
		getServiceInput := fastly.GetServiceInput{
			ID: serviceID,
		}
		service, err := client.GetService(&getServiceInput)

		if err != nil {
			log.Fatalf("Unable to fetch service %v: %v", serviceID, err)
		}

		serviceChan <- service
	}

	fmt.Println("Closing serviceChan")
	close(serviceChan)
}

func GetDictionaryItems(client *fastly.Client, service *fastly.Service, dictionaryItemChan chan<- *DictionaryRecord) {
	var activeVersion int
	for _, v := range service.Versions {
		if v.Active {
			activeVersion = v.Number
		}
	}

	gdi := fastly.GetDictionaryInput{
		ServiceID:      service.ID,
		ServiceVersion: activeVersion,
		Name:           "hostname_to_site_id",
	}
	dictionary, err := client.GetDictionary(&gdi)
	if err != nil {
		log.Fatalf("Unable to get dictionary for %v: %v", service.Name, err)
	}

	ldii := fastly.ListDictionaryItemsInput{
		ServiceID:    service.ID,
		DictionaryID: dictionary.ID,
	}
	di, err := client.ListDictionaryItems(&ldii)
	if err != nil {
		log.Fatalf("Unable to list dictionary items for %v: %v", service.Name, err)
	}

	for _, item := range di {
		dictionaryRecord := &DictionaryRecord{
			Hostname: item.ItemKey,
			SiteID:   item.ItemValue,
			service:  service,
		}
		dictionaryItemChan <- dictionaryRecord
	}
}

func AssertSiteIDMatches(record *DictionaryRecord) {
	url := fmt.Sprintf("http://policy-docs.pantheon.io/%v", record.Hostname)
	response, err := http.Get(url)
	if err != nil {
		log.Printf("Unable to fetch policy doc for service %v hostname %v: %v", record.service.Name, record.Hostname, err)
		return
	}

	if response.StatusCode == http.StatusNotFound {
		log.Printf("Policy doc not found for service %v hostname %v", record.service.Name, record.Hostname)
		return
	}

	pdocsSiteID := response.Header.Get("x-goog-meta-pcontext-site-id")
	if record.SiteID != pdocsSiteID {
		fmt.Printf("Service: %v\nHostname: %v\nDictionary site ID: %v\nPDocs site ID:      %v\n-----\n", record.service.Name, record.Hostname, record.SiteID, pdocsSiteID)
	}
}
