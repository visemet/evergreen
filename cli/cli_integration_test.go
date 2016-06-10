	"os"
	"github.com/evergreen-ci/evergreen/model/artifact"
	"github.com/evergreen-ci/evergreen/service"
	"gopkg.in/mgo.v2/bson"
var testModulePatch = `
diff --git a/blah.md b/blah.md
new file mode 100644
index 0000000..ce01362
--- /dev/null
+++ b/blah.md
@@ -0,0 +1 @@
+hello
`

type cliTestHarness struct {
	testServer       *service.TestServer
	settingsFilePath string
}

func setupCLITestHarness() cliTestHarness {
	// create a test API server
	testServer, err := service.CreateTestServer(testConfig, nil, plugin.APIPlugins, true)
	So(err, ShouldBeNil)
	So(
		db.ClearCollections(
			task.Collection,
			user.Collection,
			patch.Collection,
			model.ProjectRefCollection,
			artifact.Collection,
		),
		ShouldBeNil)
	So(db.Clear(patch.Collection), ShouldBeNil)
	So(db.Clear(model.ProjectRefCollection), ShouldBeNil)
	So((&user.DBUser{Id: "testuser", APIKey: "testapikey", EmailAddress: "tester@mongodb.com"}).Insert(), ShouldBeNil)
	localConfBytes, err := ioutil.ReadFile("testdata/sample.yml")
	So(err, ShouldBeNil)

	projectRef := &model.ProjectRef{
		Identifier:  "sample",
		Owner:       "evergreen-ci",
		Repo:        "sample",
		RepoKind:    "github",
		Branch:      "master",
		RemotePath:  "evergreen.yml",
		LocalConfig: string(localConfBytes),
		Enabled:     true,
		BatchTime:   180,
	}
	So(projectRef.Insert(), ShouldBeNil)

	// create a settings file for the command line client
	settings := Settings{
		APIServerHost: testServer.URL + "/api",
		UIServerHost:  "http://dev-evg.mongodb.com",
		APIKey:        "testapikey",
		User:          "testuser",
	}
	settingsFile, err := ioutil.TempFile("", "settings")
	So(err, ShouldBeNil)
	settingsBytes, err := yaml.Marshal(settings)
	So(err, ShouldBeNil)
	_, err = settingsFile.Write(settingsBytes)
	So(err, ShouldBeNil)
	settingsFile.Close()
	return cliTestHarness{testServer, settingsFile.Name()}
}

func TestCLIFetchSource(t *testing.T) {
	testutil.ConfigureIntegrationTest(t, testConfig, "TestCLIFetchSource")
	Convey("with a task containing patches and modules", t, func() {
		testSetup := setupCLITestHarness()
		err := os.RemoveAll("patch-1_sample")
		So(err, ShouldBeNil)

		// first, create a patch
		patchSub := patchSubmission{"sample",
			testPatch,
			"sample patch",
			"3c7bfeb82d492dc453e7431be664539c35b5db4b",
			"all",
			[]string{"all"},
			false}

		// Set up a test patch that contains module changes
		ac, rc, _, err := getAPIClients(&Options{testSetup.settingsFilePath})
		So(err, ShouldBeNil)
		newPatch, err := ac.PutPatch(patchSub)
		So(err, ShouldBeNil)
		patches, err := ac.GetPatches(0)
		So(err, ShouldBeNil)
		err = ac.UpdatePatchModule(newPatch.Id.Hex(), "render-module", testModulePatch, "1e5232709595db427893826ce19289461cba3f75")
		So(ac.FinalizePatch(newPatch.Id.Hex()), ShouldBeNil)

		patches, err = ac.GetPatches(0)
		So(err, ShouldBeNil)
		testTask, err := task.FindOne(
			db.Query(bson.M{
				task.VersionKey:      patches[0].Version,
				task.BuildVariantKey: "ubuntu",
			}))
		So(err, ShouldBeNil)
		So(testTask, ShouldNotBeNil)

		err = fetchSource(ac, rc, "", testTask.Id, false)
		So(err, ShouldBeNil)

		fileStat, err := os.Stat("./patch-1_sample/README.md")
		So(err, ShouldBeNil)
		// If patch was applied correctly, README.md will have a non-zero size
		So(fileStat.Size, ShouldNotEqual, 0)
		// If module was fetched, "render" directory should have been created.
		// The "blah.md" file should have been created if the patch was applied successfully.
		fileStat, err = os.Stat("./patch-1_sample/modules/render-module/blah.md")
		So(err, ShouldBeNil)
		So(fileStat.Size, ShouldNotEqual, 0)

	})
}
func TestCLIFetchArtifacts(t *testing.T) {
	testutil.ConfigureIntegrationTest(t, testConfig, "TestCLIFetchArtifacts")
		testSetup := setupCLITestHarness()

		err := os.RemoveAll("abcdef-rest_task_variant_task_one")
		So(err, ShouldBeNil)
		err = os.RemoveAll("abcdef-rest_task_variant_task_two")
		So(err, ShouldBeNil)
		err = (&task.Task{
			Id:           "rest_task_test_id1",
			BuildVariant: "rest_task_variant",
			Revision:     "abcdef1234",
			DependsOn:    []task.Dependency{{TaskId: "rest_task_test_id2"}},
			DisplayName:  "task_one",
		}).Insert()
		err = (&task.Task{
			Id:           "rest_task_test_id2",
			Revision:     "abcdef1234",
			BuildVariant: "rest_task_variant",
			DependsOn:    []task.Dependency{},
			DisplayName:  "task_two",
		}).Insert()
		err = (&artifact.Entry{
			TaskId:          "rest_task_test_id1",
			TaskDisplayName: "task_one",
			Files:           []artifact.File{{Link: "http://www.google.com/robots.txt"}},
		}).Upsert()

		err = (&artifact.Entry{
			TaskId:          "rest_task_test_id2",
			TaskDisplayName: "task_two",
			Files:           []artifact.File{{Link: "http://www.google.com/humans.txt"}},
		}).Upsert()

		ac, rc, _, err := getAPIClients(&Options{testSetup.settingsFilePath})
		Convey("shallow fetch artifacts should download a single task's artifacts successfully", func() {
			err = fetchArtifacts(ac, rc, "rest_task_test_id1", true)
			So(err, ShouldBeNil)
			// downloaded file should exist where we expect
			fileStat, err := os.Stat("./abcdef-rest_task_variant_task_one/robots.txt")
			So(err, ShouldBeNil)
			So(fileStat.Size(), ShouldBeGreaterThan, 0)

			fileStat, err = os.Stat("./rest_task_variant_task_two/humans.txt")
			So(os.IsNotExist(err), ShouldBeTrue)
			Convey("deep fetch artifacts should also download artifacts from dependency", func() {
				err = fetchArtifacts(ac, rc, "rest_task_test_id1", false)
				So(err, ShouldBeNil)
				fileStat, err = os.Stat("./abcdef-rest_task_variant_task_two/humans.txt")
				So(os.IsNotExist(err), ShouldBeFalse)
			})
		})
	})
}

func TestCLIFunctions(t *testing.T) {
	testutil.ConfigureIntegrationTest(t, testConfig, "TestCLIFunctions")

	Convey("with API test server running", t, func() {
		testSetup := setupCLITestHarness()

		ac, _, _, err := getAPIClients(&Options{testSetup.settingsFilePath})
			Convey("Creating a patch without variants should be successful", func() {
				patchSub := patchSubmission{
					"sample",
					testPatch,
					"sample patch",
					"3c7bfeb82d492dc453e7431be664539c35b5db4b",
					"all",
					[]string{},
					false,
				}
				_, err := ac.PutPatch(patchSub)
				So(err, ShouldBeNil)
			})
