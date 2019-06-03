package api

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"

	"github.com/project-flogo/cli/util"
	"github.com/project-flogo/core/action"
	"github.com/project-flogo/core/app"
	"github.com/project-flogo/core/app/resource"
	"github.com/project-flogo/core/data/property"
	"github.com/project-flogo/core/data/schema"
	"github.com/project-flogo/core/support"
)

type generator struct {
	resManager *resource.Manager
	pkgs       map[string]util.Import //May be something better named ?
}

func (g *generator) ResourceManager() *resource.Manager {
	return g.resManager
}

// Generator generates code for an action
type Generator interface {
	Generate(settingsName string, imports *util.Imports, config *action.Config) (code string, err error)
}

// GenerateResource is used to determine if a resource is generated, defaults to true
type GenerateResource interface {
	Generate() bool
}

// Generate generates flogo go API code
func Generate(config *app.Config, file string) {
	if config.Type != "flogo:app" {
		panic("invalid app type")
	}

	app := generator{}

	pkgImports, _ := util.ParseImports(config.Imports)
	app.pkgs = make(map[string]util.Import)

	wd, _ := os.Getwd()
	//This should probably change....
	//Since I'm using is seperatly, it's currrentDir. Once in CLI, search for `src`

	dep := util.NewDepManager(wd)

	header := "package main\n\n"

	header += "import (\n"

	for _, val := range pkgImports {

		app.pkgs[val.CanonicalAlias()] = val

		contribDesc, err := util.GetContribDescriptorFromImport(dep, val)
		if err != nil {
			panic(err)
		}

		err = support.RegisterAlias(contribDesc.GetContribType(), val.CanonicalAlias(), val.GoImportPath())
		if err != nil {
			panic(err)
		}

		if contribDesc.GetContribType() != "activity" {
			header += fmt.Sprintf("%s \"%s\"\n", val.CanonicalAlias(), val.GoImportPath())
		} else {
			header += fmt.Sprintf("_ \"%s\"\n", val.GoImportPath())
		}

	}

	header += " \"github.com/project-flogo/core/api\" \n "
	header += " \"github.com/project-flogo/core/engine\" \n "

	for id, def := range config.Schemas {
		_, err := schema.Register(id, def)
		if err != nil {
			panic(err)
		}
	}

	schema.ResolveSchemas()

	properties := make(map[string]interface{}, len(config.Properties))
	for _, attr := range config.Properties {
		properties[attr.Name()] = attr.Value()
	}

	propertyManager := property.NewManager(properties)
	property.SetDefaultManager(propertyManager)

	resources := make(map[string]*resource.Resource, len(config.Resources))
	app.resManager = resource.NewManager(resources)

	for _, actionFactory := range action.Factories() {
		err := actionFactory.Initialize(&app)
		if err != nil {
			panic(err)
		}
	}

	output := "/*\n"
	output += fmt.Sprintf("* Name: %s\n", config.Name)
	output += fmt.Sprintf("* Type: %s\n", config.Type)
	output += fmt.Sprintf("* Version: %s\n", config.Version)
	output += fmt.Sprintf("* Description: %s\n", config.Description)
	output += fmt.Sprintf("* AppModel: %s\n", config.AppModel)
	output += "*/\n\n"

	errorCheck := func() {
		output += "if err != nil {\n"
		output += "panic(err)\n"
		output += "}\n"
	}

	output += "func main() {\n"
	output += "var err error\n"
	output += "app := api.NewApp()\n"

	for i, resConfig := range config.Resources {
		resType, err := resource.GetTypeFromID(resConfig.ID)
		if err != nil {
			panic(err)
		}

		loader := resource.GetLoader(resType)
		res, err := loader.LoadResource(resConfig)
		if err != nil {
			panic(err)
		}

		generate := true
		if g, ok := loader.(GenerateResource); ok {
			generate = g.Generate()
		}
		if generate {
			header += " \"encoding/json\" \n "
			output += fmt.Sprintf("resource%d := json.RawMessage(`%s`)\n", i, string(resConfig.Data))
			output += fmt.Sprintf("app.AddResource(\"%s\", resource%d)\n", resConfig.ID, i)
		}

		resources[resConfig.ID] = res
	}

	if len(config.Properties) > 0 {
		header += " \"github.com/project-flogo/core/data \" \n"
		for _, property := range config.Properties {
			output += fmt.Sprintf("app.AddProperty(\"%s\", data.%s, %#v)\n", property.Name(),
				property.Type().Name(), property.Value())
		}
	}

	if len(config.Channels) > 0 {

		header += " \"github.com/project-flogo/core/engine/channels\" \n "
		for i, channel := range config.Channels {
			if i == 0 {
				output += fmt.Sprintf("name, buffSize := channels.Decode(\"%s\")\n", channel)
			} else {
				output += fmt.Sprintf("name, buffSize = channels.Decode(\"%s\")\n", channel)
			}
			output += fmt.Sprintf("_, err = channels.New(name, buffSize)\n")
			errorCheck()
		}
	}

	for i, act := range config.Actions {
		var actImport util.Import
		if act.Ref[:0] == "#" {
			actImport = app.pkgs[act.Ref[1:]]
		} else {
			actImport = app.pkgs[act.Ref]
		}

		factory, settingsName := action.GetFactory(act.Ref), fmt.Sprintf("actionSettings%d", i)
		if generator, ok := factory.(Generator); ok {
			code, err := generator.Generate(settingsName, &pkgImports, act)
			if err != nil {
				panic(err)
			}
			output += "\n"
			output += code
			output += "\n"
		} else {
			output += fmt.Sprintf("%s := %#v\n", settingsName, act.Settings)
		}
		output += fmt.Sprintf("err = app.AddAction(\"%s\", &%s.Action{}, %s)\n", act.Id, actImport.CanonicalAlias(), settingsName)
		errorCheck()
	}
	for i, trigger := range config.Triggers {
		var trigImport util.Import

		if trigger.Ref[:0] == "#" {
			trigImport = app.pkgs[trigger.Ref[1:]]
		} else {
			trigImport = app.pkgs[trigger.Ref[1:]]
		}

		output += fmt.Sprintf("trg%d := app.NewTrigger(&%s.Trigger{}, %#v)\n", i, trigImport.CanonicalAlias(), trigger.Settings)
		for j, handler := range trigger.Handlers {
			output += fmt.Sprintf("handler%d_%d, err := trg%d.NewHandler(%#v)\n", i, j, i, handler.Settings)
			errorCheck()
			for k, act := range handler.Actions {
				if act.Id != "" {
					output += fmt.Sprintf("action%d_%d_%d, err := handler%d_%d.NewAction(\"%s\")\n", i, j, k, i, j, act.Id)
				} else {
					var actImport util.Import
					if act.Ref[:0] == "#" {
						actImport = app.pkgs[act.Ref[1:]]
					} else {
						actImport = app.pkgs[act.Ref]
					}

					factory, settingsName := action.GetFactory(act.Ref), fmt.Sprintf("settings%d_%d_%d", i, j, k)
					if generator, ok := factory.(Generator); ok {
						code, err := generator.Generate(settingsName, &pkgImports, act.Config)
						if err != nil {
							panic(err)
						}
						output += "\n"
						output += code
						output += "\n"
					} else {
						output += fmt.Sprintf("%s := %#v\n", settingsName, act.Settings)
					}
					output += fmt.Sprintf("action%d_%d_%d, err := handler%d_%d.NewAction(&%s.Action{}, %s)\n", i, j, k, i, j, actImport.CanonicalAlias(), settingsName)
				}
				errorCheck()
				if act.If != "" {
					output += fmt.Sprintf("action%d_%d_%d.SetCondition(\"%s\")\n", i, j, k, act.If)
				}
				if length := len(act.Input); length > 0 {
					mappings := make([]string, 0, length)
					for key, value := range act.Input {
						mappings = append(mappings, fmt.Sprintf("%s%v", key, value))
					}
					output += fmt.Sprintf("action%d_%d_%d.SetInputMappings(%#v...)\n", i, j, k, mappings)
				}
				if length := len(act.Output); length > 0 {
					mappings := make([]string, 0, length)
					for key, value := range act.Output {
						mappings = append(mappings, fmt.Sprintf("%s%v", key, value))
					}
					output += fmt.Sprintf("action%d_%d_%d.SetOutputMappings(%#v...)\n", i, j, k, mappings)
				}
				//output += fmt.Sprintf("_ = action%d_%d_%d\n", i, j, k) Do we need this ??
			}
			//output += fmt.Sprintf("_ = handler%d_%d\n", i, j) Do we need this ??
		}
		//output += fmt.Sprintf("_ = trg%d\n", i) Do we need this ??
	}

	output += "e, err := api.NewEngine(app)\n"
	errorCheck()

	output += "engine.RunEngine(e)\n"
	output += "}\n"

	header += "\n )\n"
	output = header + output

	out, err := os.Create(file)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	buffer := bytes.NewBufferString(output)
	fileSet := token.NewFileSet()
	code, err := parser.ParseFile(fileSet, file, buffer, parser.ParseComments)
	if err != nil {
		buffer.WriteTo(out)
		panic(fmt.Errorf("%v: %v", file, err))
	}

	formatter := printer.Config{Mode: printer.TabIndent | printer.UseSpaces, Tabwidth: 8}
	err = formatter.Fprint(out, fileSet, code)
	if err != nil {
		buffer.WriteTo(out)
		panic(fmt.Errorf("%v: %v", file, err))
	}
}
